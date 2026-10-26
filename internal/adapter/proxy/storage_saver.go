package proxy

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/mobileview"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/state"
)

const provenanceKey = "storage-saver-source"

var errDuplicateInTrash = errors.New("matching Immich asset is trashed")

type storageSaver struct {
	store       *state.Store
	dir, preset string
	mobileView  bool
}

func (s *Server) EnableStorageSaver(dir, preset string, mobile bool) error {
	ledger, err := state.Open(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "spool"), 0700); err != nil {
		return err
	}
	s.saver = &storageSaver{store: ledger, dir: dir, preset: preset, mobileView: mobile}
	// One writer also serializes retries for the same source and bounds spool usage.
	s.semaphore = make(chan struct{}, 1)
	return nil
}

func (s *Server) upstreamRequest(r *http.Request, method, path string, body io.Reader) (*http.Request, error) {
	u := *s.upstreamURL
	u.Path = path
	u.RawQuery = ""
	req, err := http.NewRequestWithContext(r.Context(), method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"Authorization", "Cookie", "x-api-key", "x-immich-user-token", "x-immich-session-token"} {
		if value := r.Header.Get(key); value != "" {
			req.Header.Set(key, value)
		}
	}
	return req, nil
}

func (s *Server) owner(r *http.Request) (string, error) {
	req, err := s.upstreamRequest(r, "GET", "/api/users/me", nil)
	if err != nil {
		return "", err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("authentication HTTP %d", resp.StatusCode)
	}
	var user struct {
		ID string `json:"id"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&user)
	if err != nil || user.ID == "" {
		return "", fmt.Errorf("invalid authenticated user response")
	}
	return user.ID, nil
}

// checkOriginalDuplicate asks Immich whether the authenticated owner already
// has the original bytes. This must run before conversion so a stock mobile
// client's existing JPEG is not converted or uploaded again.
func (s *Server) checkOriginalDuplicate(r *http.Request, sourceSHA1 string) (string, error) {
	body := fmt.Sprintf(`{"assets":[{"id":%q,"checksum":%q}]}`, sourceSHA1, sourceSHA1)
	req, err := s.upstreamRequest(r, http.MethodPost, "/api/assets/bulk-upload-check", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("bulk upload check: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return "", fmt.Errorf("read bulk upload check response: %w", err)
	}
	if len(data) > 1<<20 {
		return "", errors.New("bulk upload check response exceeds limit")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bulk upload check HTTP %d", resp.StatusCode)
	}
	results, err := mobileview.ParseBulkUploadCheckResponse(data)
	if err != nil {
		return "", fmt.Errorf("invalid bulk upload check response: %w", err)
	}
	if len(results) != 1 {
		return "", fmt.Errorf("bulk upload check returned %d results, want 1", len(results))
	}
	result := results[0]
	if result.ID != sourceSHA1 {
		return "", fmt.Errorf("bulk upload check returned unrelated asset ID %q", result.ID)
	}
	switch result.Action {
	case "accept":
		return "", nil
	case "reject":
		if result.Reason != "duplicate" || result.AssetID == "" {
			return "", fmt.Errorf("bulk upload check rejected the original without a reusable duplicate")
		}
		if result.IsTrashed == nil {
			return "", errors.New("bulk upload check omitted duplicate trash state")
		}
		if *result.IsTrashed {
			return "", fmt.Errorf("%w (asset %s); restore or remove it before retrying", errDuplicateInTrash, result.AssetID)
		}
		return result.AssetID, nil
	default:
		return "", fmt.Errorf("bulk upload check returned unsupported action %q", result.Action)
	}
}

func fileSHA1(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha1.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// Copy only via a temporary sibling, so a failed write never looks like completed work.
func durableCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".copy-*")
	if err != nil {
		return err
	}
	name := out.Name()
	defer os.Remove(name)
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, dst); err != nil {
		return err
	}
	if d, e := os.Open(filepath.Dir(dst)); e == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *Server) serveVerifiedUpload(w http.ResponseWriter, r *http.Request) {
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
	if err = s.verifiedUpload(w, r, owner); err != nil {
		s.logger.Printf("Verified ingestion failed: %v", err)
		if errors.Is(err, errDuplicateInTrash) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "Upload not acknowledged; staged source retained for retry", http.StatusBadGateway)
	}
}

func (s *Server) verifiedUpload(w http.ResponseWriter, r *http.Request, owner string) error {
	// The experiment is a bounded synchronous upload, not a fabricated background ACK.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return err
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File[s.filterFormKey]
	if len(files) != 1 {
		return fmt.Errorf("exactly one assetData file is required")
	}
	header := files[0]
	if !s.shouldOptimizeExt(filepath.Ext(header.Filename)) {
		return fmt.Errorf("format not configured")
	}
	in, err := header.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Join(s.saver.dir, "spool"), ".incoming-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	hash := sha1.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), in)
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	sourceHash := hex.EncodeToString(hash.Sum(nil))
	ownerHash := sha256.Sum256([]byte(owner))
	prefix := hex.EncodeToString(ownerHash[:8]) + "-" + sourceHash
	src := filepath.Join(s.saver.dir, "spool", prefix+strings.ToLower(filepath.Ext(header.Filename)))
	record, exists := s.saver.store.FindSource(owner, sourceHash)
	if exists && record.AssetID != "" && record.State == "verified" {
		if err := s.verifyRemote(r, record); err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			return json.NewEncoder(w).Encode(map[string]string{"id": record.AssetID, "status": "duplicate"})
		}
		// Do not silently overwrite aliases after deletion or a server integrity failure.
		return fmt.Errorf("previously recorded asset unavailable; reconcile before retry")
	}
	duplicateID, err := s.checkOriginalDuplicate(r, sourceHash)
	if err != nil {
		return err
	}
	if duplicateID != "" {
		filename := filepath.Base(header.Filename)
		identity := state.Record{
			OwnerID:        owner,
			AssetID:        duplicateID,
			SourceSHA1:     sourceHash,
			OutputSHA1:     sourceHash,
			SourceFilename: filename,
			OutputFilename: filename,
			Preset:         "identity",
			State:          "verified",
			SourceSize:     n,
			OutputSize:     n,
			UpdatedAt:      time.Now().UTC(),
		}
		if err = s.verifyRemote(r, identity); err != nil {
			return fmt.Errorf("verify existing duplicate: %w", err)
		}
		if err = s.saver.store.Put(identity); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		return json.NewEncoder(w).Encode(map[string]string{"id": duplicateID, "status": "duplicate"})
	}
	if err = durableCopy(tmpPath, src); err != nil {
		return err
	}
	if !exists {
		record = state.Record{OwnerID: owner, SourceSHA1: sourceHash, SourceFilename: filepath.Base(header.Filename), SourceSize: n, Preset: s.saver.preset}
	}
	record.State = "staged"
	record.UpdatedAt = time.Now().UTC()
	// Reuse a durable output across retries, even if codec versions later change.
	output := record.SpoolPath
	validOutput := false
	if output != "" && record.OutputSHA1 != "" {
		h, _, e := fileSHA1(output)
		validOutput = e == nil && h == record.OutputSHA1
	}
	if !validOutput {
		if err = s.saver.store.Put(record); err != nil {
			return err
		}
		result, e := s.processor.Process(r.Context(), src, s.tasks)
		if e != nil {
			return e
		}
		if result != nil && result.Cleanup != nil {
			defer result.Cleanup()
		}
		candidate := src
		outName := record.SourceFilename
		if result != nil && result.ProcessedSize > 0 && result.ProcessedSize < n && result.ProcessedFilePath != "" {
			candidate = result.ProcessedFilePath
			outName = strings.TrimSuffix(record.SourceFilename, filepath.Ext(record.SourceFilename)) + filepath.Ext(result.ProcessedFilename)
		}
		output = filepath.Join(s.saver.dir, "spool", prefix+"-output"+strings.ToLower(filepath.Ext(outName)))
		if err = durableCopy(candidate, output); err != nil {
			return err
		}
		record.OutputFilename = outName
		record.OutputSHA1, record.OutputSize, err = fileSHA1(output)
		if err != nil {
			return err
		}
		record.SpoolPath = output
		if err = s.saver.store.Put(record); err != nil {
			return err
		}
	}
	metadata := []map[string]any{}
	if raw := r.FormValue("metadata"); raw != "" {
		if err = json.Unmarshal([]byte(raw), &metadata); err != nil {
			return fmt.Errorf("invalid client metadata")
		}
	}
	for _, m := range metadata {
		if m["key"] == provenanceKey {
			return fmt.Errorf("reserved provenance metadata key")
		}
	}
	metadata = append(metadata, map[string]any{"key": provenanceKey, "value": map[string]any{
		"schemaVersion": 1, "ownerId": owner, "sourceSHA1": sourceHash, "outputSHA1": record.OutputSHA1,
		"sourceFilename": record.SourceFilename, "outputFilename": record.OutputFilename, "preset": record.Preset,
		"sourceSize": record.SourceSize, "outputSize": record.OutputSize}})
	metaJSON, _ := json.Marshal(metadata)
	body, err := os.CreateTemp(filepath.Join(s.saver.dir, "spool"), ".multipart-*")
	if err != nil {
		return err
	}
	defer func() { body.Close(); os.Remove(body.Name()) }()
	mw := multipart.NewWriter(body)
	for key, values := range r.MultipartForm.Value {
		if key == "metadata" || key == "filename" {
			continue
		}
		for _, value := range values {
			if err = mw.WriteField(key, value); err != nil {
				return err
			}
		}
	}
	if err = mw.WriteField("filename", record.OutputFilename); err != nil {
		return err
	}
	if err = mw.WriteField("metadata", string(metaJSON)); err != nil {
		return err
	}
	for key, parts := range r.MultipartForm.File {
		for _, part := range parts {
			name := part.Filename
			var f io.ReadCloser
			if key == s.filterFormKey {
				name = record.OutputFilename
				f, err = os.Open(output)
			} else {
				f, err = part.Open()
			}
			if err != nil {
				return err
			}
			mt := mime.TypeByExtension(filepath.Ext(name))
			if mt == "" {
				mt = "application/octet-stream"
			}
			ph := make(textproto.MIMEHeader)
			ph.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": key, "filename": name}))
			ph.Set("Content-Type", mt)
			pw, e := mw.CreatePart(ph)
			if e == nil {
				_, e = io.Copy(pw, f)
			}
			f.Close()
			if e != nil {
				return e
			}
		}
	}
	if err = mw.Close(); err != nil {
		return err
	}
	info, err := body.Stat()
	if err != nil {
		return err
	}
	if _, err = body.Seek(0, 0); err != nil {
		return err
	}
	req, err := s.upstreamRequest(r, "POST", "/api/assets", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = info.Size()
	// Do not forward the source checksum header after changing the file bytes.
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	response, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("upstream upload HTTP %d", resp.StatusCode)
	}
	var uploaded struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(response, &uploaded); err != nil || uploaded.ID == "" {
		return fmt.Errorf("missing upload asset ID")
	}
	record.AssetID = uploaded.ID
	record.State = "uploaded"
	record.UpdatedAt = time.Now().UTC()
	if err = s.saver.store.Put(record); err != nil {
		return err
	}
	if err = s.verifyRemote(r, record); err != nil {
		return err
	}
	// A duplicate response may not attach metadata; ensure the backup copy is present.
	update := map[string]any{"items": []map[string]any{metadata[len(metadata)-1]}}
	b, _ := json.Marshal(update)
	metaReq, _ := s.upstreamRequest(r, "PUT", "/api/assets/"+record.AssetID+"/metadata", bytes.NewReader(b))
	metaReq.Header.Set("Content-Type", "application/json")
	metaResp, err := s.client.Do(metaReq)
	if err != nil {
		return err
	}
	metaResp.Body.Close()
	if metaResp.StatusCode/100 != 2 {
		return fmt.Errorf("metadata backup HTTP %d", metaResp.StatusCode)
	}
	record.State = "verified"
	record.UpdatedAt = time.Now().UTC()
	if err = s.saver.store.Put(record); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(response)
	return err
}

func (s *Server) verifyRemote(r *http.Request, record state.Record) error {
	req, err := s.upstreamRequest(r, "GET", "/api/assets/"+record.AssetID+"/original", nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("remote verification HTTP %d", resp.StatusCode)
	}
	h := sha1.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != record.OutputSHA1 || n != record.OutputSize {
		return fmt.Errorf("remote content checksum/size mismatch")
	}
	return nil
}

func (s *Server) serveMobileView(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	// Transform metadata and sync only. Original/media downloads remain byte-exact.
	if strings.Contains(r.URL.Path, "/original") || strings.Contains(r.URL.Path, "/thumbnail") || strings.Contains(r.URL.Path, "/video/") {
		return false
	}
	owner, err := s.owner(r)
	if err != nil {
		return false
	}
	lookup := func(owner, id string) (string, bool) {
		rec, ok := s.saver.store.FindAsset(owner, id)
		return rec.SourceSHA1, ok && rec.State == "verified"
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check" {
		data, readErr := io.ReadAll(io.LimitReader(r.Body, (8<<20)+1))
		if readErr != nil || len(data) > 8<<20 {
			http.Error(w, "invalid bulk check body", http.StatusBadRequest)
			return true
		}
		data, err = mobileview.RewriteBulkUploadCheckRequest(data, owner, func(owner, source string) (string, bool) {
			rec, ok := s.saver.store.FindSource(owner, source)
			return rec.OutputSHA1, ok && rec.State == "verified"
		})
		if err != nil {
			http.Error(w, "invalid bulk check request", http.StatusBadRequest)
			return true
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		r.ContentLength = int64(len(data))
		r.Header.Set("Content-Length", fmt.Sprint(len(data)))
	}
	proxy := *s.proxy
	proxy.ModifyResponse = func(resp *http.Response) error {
		ct := resp.Header.Get("Content-Type")
		if strings.Contains(ct, "ndjson") || strings.Contains(ct, "jsonlines+json") {
			reader, writer := io.Pipe()
			old := resp.Body
			go func() {
				defer old.Close()
				err := mobileview.RewriteNDJSON(writer, old, owner, lookup)
				writer.CloseWithError(err)
			}()
			resp.Body = reader
			resp.ContentLength = -1
			resp.Header.Del("Content-Length")
			return nil
		}
		if !strings.Contains(ct, "application/json") {
			return nil
		}
		defer resp.Body.Close()
		data, e := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
		if e != nil {
			return e
		}
		if len(data) > 32<<20 {
			return fmt.Errorf("JSON response exceeds rewrite limit")
		}
		data, e = mobileview.RewriteJSON(data, owner, lookup)
		if e != nil {
			return e
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
		resp.ContentLength = int64(len(data))
		resp.Header.Set("Content-Length", fmt.Sprint(len(data)))
		resp.Header.Del("ETag")
		return nil
	}
	r.Header.Del("Accept-Encoding")
	proxy.ServeHTTP(w, r)
	return true
}
