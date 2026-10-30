package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/colthreepv/immich-optimizer/internal/adapter/state"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

// StorageSaverStore returns the already-open alias ledger; callers must not
// open a second writer for the same directory.
func (s *Server) StorageSaverStore() *state.Store {
	if s.saver == nil {
		return nil
	}
	return s.saver.store
}

// OriginalFirstQueue binds the proxy to the queue without starting workers or
// the publication loop. FindIntent must only return jobs created by an
// earlier ingestion of this owner's source, including pending intents.
type OriginalFirstQueue struct {
	Enqueue              func(jobqueue.Input, io.Reader) (jobqueue.Job, bool, error)
	FindIntent           func(owner, sourceSHA256 string) (jobqueue.Job, bool, error)
	MarkOriginalUploaded func(id, assetID string) error
}

// EnableOriginalFirst switches only upload requests to original-first mode.
// EnableStorageSaver must already have opened the sole alias ledger writer.
func (s *Server) EnableOriginalFirst(queue OriginalFirstQueue, apiKey string) error {
	if s.saver == nil || queue.Enqueue == nil || queue.FindIntent == nil || queue.MarkOriginalUploaded == nil || strings.TrimSpace(apiKey) == "" {
		return errors.New("original-first requires storage saver, queue callbacks and an owner-scoped API key")
	}
	s.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.isUploadRequest(r) {
			s.ServeHTTP(w, r)
			return
		}
		s.serveOriginalFirst(w, r, queue, apiKey)
	})
	return nil
}

func (s *Server) serveOriginalFirst(w http.ResponseWriter, r *http.Request, queue OriginalFirstQueue, apiKey string) {
	owner, err := s.owner(r)
	if err != nil {
		http.Error(w, "Immich authentication required", http.StatusUnauthorized)
		return
	}
	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	case <-r.Context().Done():
		return
	}
	if err = s.originalFirstUpload(w, r, owner, queue, apiKey); err != nil {
		s.logger.Printf("Original-first ingestion failed: %v", err)
		if errors.Is(err, errDuplicateInTrash) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "Upload not acknowledged; retry safely", http.StatusBadGateway)
	}
}

func (s *Server) originalFirstUpload(w http.ResponseWriter, r *http.Request, owner string, queue OriginalFirstQueue, apiKey string) error {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	dir, err := os.MkdirTemp(filepath.Join(s.saver.dir, "spool"), ".original-first-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	parts, err := captureOriginalFirstParts(r, dir, s.filterFormKey)
	if err != nil {
		return err
	}

	// Old verified aliases take priority over native original checks. Never
	// rewrite the ledger merely because an original-first retry occurred.
	if record, exists := s.saver.store.FindSource(owner, parts.SourceSHA1); exists && record.State == "verified" && record.AssetID != "" {
		if err := s.verifyRemote(r, record); err != nil {
			return fmt.Errorf("previous alias unavailable: %w", err)
		}
		return writeOriginalFirstDuplicate(w, record.AssetID)
	}

	duplicateID, err := s.checkOriginalDuplicate(r, parts.SourceSHA1)
	if err != nil {
		return err
	}
	job, found, err := queue.FindIntent(owner, parts.SourceSHA256)
	queueAvailable := err == nil
	if err != nil {
		s.logger.Printf("Optimization missed: cannot check queue intent: %v", err)
		found = false
	}
	if duplicateID != "" {
		if found && job.OriginalAssetID != "" && job.OriginalAssetID != duplicateID {
			s.logger.Printf("Optimization missed: native duplicate %s differs from queued original %s", duplicateID, job.OriginalAssetID)
			return writeOriginalFirstDuplicate(w, duplicateID)
		}
		if found && job.OriginalAssetID == "" {
			if err := queue.MarkOriginalUploaded(job.ID, duplicateID); err != nil {
				s.logger.Printf("Optimization deferred: cannot record existing original %s: %v", duplicateID, err)
			}
		}
		return writeOriginalFirstDuplicate(w, duplicateID)
	}
	if found && job.OriginalAssetID != "" {
		s.logger.Printf("Optimization missed: queued original %s is no longer present", job.OriginalAssetID)
		queueAvailable = false
	}

	// Videos and unsupported formats remain ordinary original uploads.
	// No background publisher can act on an asset owned by another user.
	eligible := queueAvailable && s.originalFirstEligible(parts.SourceFilename)
	if eligible {
		serviceRequest := r.Clone(r.Context())
		serviceRequest.Header = make(http.Header)
		serviceRequest.Header.Set("x-api-key", apiKey)
		serviceOwner, err := s.owner(serviceRequest)
		if err != nil || serviceOwner != owner {
			s.logger.Printf("Optimization missed: background API key does not identify the upload owner")
			eligible = false
		}
	}
	if eligible {
		fields := job.UploadFields
		if !found {
			fields, err = originalFirstUploadFields(s.saver.dir, parts)
			if err != nil {
				s.logger.Printf("Optimization missed: cannot persist multipart metadata: %v", err)
				eligible = false
			}
		}
		if eligible {
			file, err := os.Open(parts.SourcePath)
			if err != nil {
				s.logger.Printf("Optimization missed: cannot open staged source: %v", err)
				eligible = false
			} else {
				job, _, err = queue.Enqueue(jobqueue.Input{OwnerID: owner, SourceSHA1: parts.SourceSHA1, SourceSHA256: parts.SourceSHA256, Size: parts.SourceSize, Filename: parts.SourceFilename, Preset: s.saver.preset, UploadFields: fields}, file)
				_ = file.Close()
				if err != nil {
					s.logger.Printf("Optimization missed: queue admission failed: %v", err)
					eligible = false
				}
			}
		}
	}

	body, err := os.CreateTemp(dir, "upload-*")
	if err != nil {
		return err
	}
	defer body.Close()
	mw := multipart.NewWriter(body)
	if err = writeOriginalFirstParts(mw, parts.Parts); err != nil {
		return err
	}
	info, err := body.Stat()
	if err != nil {
		return err
	}
	if _, err = body.Seek(0, io.SeekStart); err != nil {
		return err
	}
	req, err := s.upstreamRequest(r, http.MethodPost, r.URL.Path, body)
	if err != nil {
		return err
	}
	req.URL.RawQuery = r.URL.RawQuery
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = info.Size()
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(response) > 1<<20 || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) {
		return fmt.Errorf("original upload failed: HTTP %d", resp.StatusCode)
	}
	var uploaded struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(response, &uploaded); err != nil || uploaded.ID == "" {
		return errors.New("original upload returned no asset ID")
	}
	if eligible {
		if err := queue.MarkOriginalUploaded(job.ID, uploaded.ID); err != nil {
			s.logger.Printf("Optimization deferred: cannot record uploaded original %s: %v", uploaded.ID, err)
		}
	}
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, bytes.NewReader(response))
	return err
}

func writeOriginalFirstDuplicate(w http.ResponseWriter, id string) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "duplicate"})
}

func (s *Server) originalFirstEligible(filename string) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(filename)), ".")
	// The currently supported asynchronous recipe is JPEG to AVIF only.
	switch ext {
	case "jpg", "jpeg", "jpe", "jfif":
	default:
		return false
	}
	for _, task := range s.tasks {
		if strings.TrimSpace(task.Command) == "" {
			continue
		}
		for _, configured := range task.Extensions {
			if strings.TrimPrefix(strings.ToLower(configured), ".") == ext {
				return true
			}
		}
	}
	return false
}
