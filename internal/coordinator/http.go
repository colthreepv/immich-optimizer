package coordinator

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

// WorkerHandler exposes only leased artifacts, never coordinator paths or credentials.
type WorkerHandler struct {
	Queue  *jobqueue.Store
	Token  string
	Paused bool
}

func (h *WorkerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
		return
	}
	authorization := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authorization, "Bearer ")
	if !strings.HasPrefix(authorization, "Bearer ") || h.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.Token)) != 1 {
		http.Error(w, "worker authentication required", http.StatusUnauthorized)
		return
	}
	worker := r.Header.Get("X-Worker-ID")
	if worker == "" || len(worker) > 128 {
		http.Error(w, "worker ID required", http.StatusBadRequest)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/internal/jobs" {
		jobs := make([]map[string]any, 0)
		for _, job := range h.Queue.List() {
			jobs = append(jobs, map[string]any{"id": job.ID, "status": job.Status, "publicationStage": job.PublicationStage, "originalAssetId": job.OriginalAssetID, "resultAssetId": job.ResultAssetID, "filename": job.Filename, "error": job.LastError, "attempts": job.Attempts})
		}
		respond(w, http.StatusOK, map[string]any{"jobs": jobs})
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/internal/jobs/claim" {
		if h.Paused {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.claim(w, r, worker)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/internal/jobs/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/internal/jobs/") || len(parts) != 2 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	id, action := parts[0], parts[1]
	if action == "resume" && r.Method == http.MethodPost {
		job, err := h.Queue.ResumePublication(id)
		if err != nil {
			queueError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]any{"id": job.ID, "status": job.Status})
		return
	}
	lease := r.Header.Get("X-Lease-Token")
	if lease == "" {
		http.Error(w, "lease token required", http.StatusBadRequest)
		return
	}
	switch {
	case action == "source" && r.Method == http.MethodGet:
		source, err := h.Queue.OpenSource(id, worker, lease)
		if err != nil {
			queueError(w, err)
			return
		}
		defer source.Close()
		job, err := h.Queue.Get(id)
		if err != nil {
			queueError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(job.Size, 10))
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.Copy(w, source)
	case action == "heartbeat" && r.Method == http.MethodPost:
		if r.ContentLength != 0 {
			var body jobqueue.LeaseRequest
			if !decode(w, r, &body) {
				return
			}
			if body.WorkerID != worker || body.Token != lease {
				http.Error(w, "lease identity mismatch", http.StatusConflict)
				return
			}
		}
		job, err := h.Queue.Heartbeat(id, worker, lease)
		if err != nil {
			queueError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]any{"expiresAt": job.Lease.ExpiresAt})
	case action == "result" && r.Method == http.MethodPut:
		name := r.Header.Get("X-Output-Filename")
		if name == "" || strings.ContainsAny(name, "/\\\r\n") || !strings.HasSuffix(strings.ToLower(name), ".avif") {
			http.Error(w, "AVIF filename required", http.StatusBadRequest)
			return
		}
		size, err := strconv.ParseInt(r.Header.Get("X-Output-Size"), 10, 64)
		if err != nil || size <= 0 || size > 2<<30 {
			http.Error(w, "invalid result size", http.StatusBadRequest)
			return
		}
		if r.ContentLength >= 0 && r.ContentLength != size {
			http.Error(w, "result length mismatch", http.StatusBadRequest)
			return
		}
		input := jobqueue.ResultInput{SHA1: r.Header.Get("X-Output-SHA1"), SHA256: r.Header.Get("X-Output-SHA256"), Size: size, Filename: r.Header.Get("X-Output-Filename")}
		r.Body = http.MaxBytesReader(w, r.Body, size+1)
		job, err := h.Queue.Complete(id, worker, lease, input, r.Body)
		if err != nil {
			queueError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]any{"id": job.ID, "status": job.Status})
	case action == "fail" && r.Method == http.MethodPost:
		var body struct {
			WorkerID  string `json:"workerId"`
			Token     string `json:"token"`
			Error     string `json:"error"`
			Retryable bool   `json:"retryable"`
			Skipped   bool   `json:"skipped"`
		}
		if !decode(w, r, &body) {
			return
		}
		if body.WorkerID != worker || body.Token != lease {
			http.Error(w, "lease identity mismatch", http.StatusConflict)
			return
		}
		if len(body.Error) > 4096 {
			body.Error = body.Error[:4096]
		}
		var err error
		if body.Skipped || !body.Retryable {
			_, err = h.Queue.MarkSkipped(id, worker, lease, body.Error)
		} else {
			_, err = h.Queue.Fail(id, worker, lease, body.Error)
		}
		if err != nil {
			queueError(w, err)
			return
		}
		respond(w, http.StatusOK, map[string]string{"id": id, "status": "recorded"})
	default:
		http.NotFound(w, r)
	}
}

func (h *WorkerHandler) claim(w http.ResponseWriter, r *http.Request, worker string) {
	var body jobqueue.ClaimRequest
	if !decode(w, r, &body) {
		return
	}
	if body.WorkerID != worker {
		http.Error(w, "worker identity mismatch", http.StatusBadRequest)
		return
	}
	job, err := h.Queue.Claim(worker)
	if errors.Is(err, jobqueue.ErrNoJobs) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		queueError(w, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"id": job.ID, "filename": job.Filename, "preset": job.Preset, "sourceSha1": job.SourceSHA1, "sourceSha256": job.SourceSHA256, "sourceSize": job.Size, "leaseToken": job.Lease.Token, "expiresAt": job.Lease.ExpiresAt})
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		http.Error(w, "one JSON object required", http.StatusBadRequest)
		return false
	}
	return true
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func queueError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, jobqueue.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, jobqueue.ErrLease), errors.Is(err, jobqueue.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, jobqueue.ErrLimit):
		status = http.StatusInsufficientStorage
	}
	http.Error(w, fmt.Sprintf("queue operation failed: %s", http.StatusText(status)), status)
}
