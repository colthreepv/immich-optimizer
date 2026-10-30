package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
	customlogger "github.com/colthreepv/immich-optimizer/internal/logger"
)

func TestReconcileOriginalIntentWithoutClientRetry(t *testing.T) {
	var uploads, checks int
	var existing bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "owner-key" {
			if r.URL.Path == "/api/users/me" {
				io.WriteString(w, `{"id":"other-owner"}`)
				return
			}
			t.Error("wrong owner key reached another endpoint")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/users/me":
			io.WriteString(w, `{"id":"owner"}`)
		case "/api/assets/bulk-upload-check":
			checks++
			var request struct {
				Assets []struct {
					ID string `json:"id"`
				} `json:"assets"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			result := map[string]any{"id": request.Assets[0].ID, "action": "accept"}
			if existing {
				result = map[string]any{"id": request.Assets[0].ID, "action": "reject", "reason": "duplicate", "assetId": "real-id", "isTrashed": false}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{result}})
		case "/api/assets":
			uploads++
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			if r.FormValue("deviceAssetId") != "device-id" {
				t.Error("missing upload metadata")
			}
			for field, want := range map[string]string{"assetData": "original", "sidecarData": "sidecar"} {
				f, _, err := r.FormFile(field)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(f)
				f.Close()
				if string(b) != want {
					t.Errorf("%s = %q", field, b)
				}
			}
			existing = true
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"real-id"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	store, err := jobqueue.Open(filepath.Join(dir, "queue"), jobqueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("deviceAssetId", "device-id")
	for field, data := range map[string]string{"assetData": "original", "sidecarData": "sidecar"} {
		f, _ := mw.CreateFormFile(field, field+".jpg")
		_, _ = f.Write([]byte(data))
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	parts, err := captureOriginalFirstParts(r, t.TempDir(), "assetData")
	if err != nil {
		t.Fatal(err)
	}
	fields, err := originalFirstUploadFields(dir, parts)
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(parts.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	job, _, err := store.Enqueue(jobqueue.Input{OwnerID: "owner", SourceSHA1: parts.SourceSHA1, SourceSHA256: parts.SourceSHA256, Size: parts.SourceSize, Filename: parts.SourceFilename, Preset: "preset", UploadFields: fields}, source)
	_ = source.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = jobqueue.Open(filepath.Join(dir, "queue"), jobqueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	job, err = store.Get(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(upstream.URL)
	s := NewServer(u, ":0", "/api/assets", "assetData", nil, nil, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
	if err := s.EnableStorageSaver(dir, "preset", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileOriginalIntent(context.Background(), job, "wrong-owner-key"); err == nil {
		t.Fatal("accepted wrong owner key")
	}
	if checks != 0 || uploads != 0 {
		t.Fatal("owner mismatch reached duplicate check or upload")
	}
	id, err := s.ReconcileOriginalIntent(context.Background(), job, "owner-key")
	if err != nil || id != "real-id" {
		t.Fatalf("reconcile: %q %v", id, err)
	}
	// If the checkpoint is interrupted, the next background poll must reuse A.
	id, err = s.ReconcileOriginalIntent(context.Background(), job, "owner-key")
	if err != nil || id != "real-id" || uploads != 1 {
		t.Fatalf("duplicate reconciliation: %q %v uploads=%d", id, err, uploads)
	}
	if _, err := store.MarkOriginalUploaded(job.ID, id); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileRejectsManifestOutsideManagedRoot(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(&url.URL{Scheme: "http", Host: "example.invalid"}, ":0", "/api/assets", "assetData", nil, nil, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
	if err := s.EnableStorageSaver(dir, "preset", false); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "original-first-parts"), 0700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(other, []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reconcileParts(jobqueue.Job{Input: jobqueue.Input{UploadFields: map[string][]string{originalFirstMultipartField: {other}}}}); err == nil {
		t.Fatal("accepted manifest outside managed root")
	}
}

func TestReconcileRejectsEscapingSidecarAndTrashedDuplicate(t *testing.T) {
	dir := t.TempDir()
	jobDir := filepath.Join(dir, "original-first-parts", "job-test")
	if err := os.MkdirAll(jobDir, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "part-0")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(jobDir, "manifest.json")
	data, _ := json.Marshal([]originalFirstPart{{Header: map[string][]string{"Content-Disposition": {`form-data; name="sidecarData"; filename="x.xmp"`}}, Path: outside}})
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			io.WriteString(w, `{"id":"owner"}`)
		case "/api/assets/bulk-upload-check":
			var body struct {
				Assets []struct {
					ID string `json:"id"`
				} `json:"assets"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": body.Assets[0].ID, "action": "reject", "reason": "duplicate", "assetId": "trashed-id", "isTrashed": true}}})
		default:
			t.Errorf("unexpected upstream request: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	s := NewServer(u, ":0", "/api/assets", "assetData", nil, nil, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
	if err := s.EnableStorageSaver(dir, "preset", false); err != nil {
		t.Fatal(err)
	}
	job := jobqueue.Job{Input: jobqueue.Input{OwnerID: "owner", SourceSHA1: legacyDigest([]byte("source")), UploadFields: map[string][]string{originalFirstMultipartField: {manifest}}}, Status: jobqueue.StatusPublishingOriginal}
	if _, err := s.reconcileParts(job); err == nil {
		t.Fatal("accepted sidecar outside manifest directory")
	}
	if _, err := s.ReconcileOriginalIntent(context.Background(), job, "key"); !errors.Is(err, errDuplicateInTrash) {
		t.Fatalf("trashed duplicate error: %v", err)
	}
}
