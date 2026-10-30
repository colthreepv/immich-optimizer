package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colthreepv/immich-optimizer/internal/adapter/state"
	"github.com/colthreepv/immich-optimizer/internal/domain/entity"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
	customlogger "github.com/colthreepv/immich-optimizer/internal/logger"
)

func TestOriginalFirstRecoversUploadBeforeQueueTransition(t *testing.T) {
	var job jobqueue.Job
	var sourceStored []byte
	var exists, uploaded, failMark bool
	failMark = true
	q := OriginalFirstQueue{
		FindIntent: func(owner, sha string) (jobqueue.Job, bool, error) {
			return job, exists && job.OwnerID == owner && job.SourceSHA256 == sha, nil
		},
		Enqueue: func(input jobqueue.Input, r io.Reader) (jobqueue.Job, bool, error) {
			var err error
			sourceStored, err = io.ReadAll(r)
			job = jobqueue.Job{Input: input, ID: "job-1"}
			exists = true
			return job, true, err
		},
		MarkOriginalUploaded: func(id, asset string) error {
			if failMark {
				failMark = false
				return errors.New("simulated crash")
			}
			job.OriginalAssetID = asset
			return nil
		},
	}
	var uploadCount int
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
			if uploaded {
				json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": body.Assets[0].ID, "action": "reject", "reason": "duplicate", "assetId": "real-id", "isTrashed": false}}})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": body.Assets[0].ID, "action": "accept"}}})
			}
		case "/api/assets":
			if !exists || !bytes.Equal(sourceStored, []byte("source")) {
				t.Error("original uploaded before source intent was persisted")
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			f, _, err := r.FormFile("sidecarData")
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			f.Close()
			if string(b) != "sidecar" {
				t.Fatalf("sidecar = %q", b)
			}
			uploaded = true
			uploadCount++
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"real-id","status":"created"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	processor := &mockProcessor{processFunc: func(context.Context, string, []entity.Task) (*entity.ProcessResult, error) {
		t.Fatal("unexpected local conversion")
		return nil, nil
	}}
	var logs bytes.Buffer
	s := NewServer(u, ":0", "/api/assets", "assetData", processor, []entity.Task{{Extensions: []string{"jpg"}, Command: "recipe"}}, customlogger.NewWithLevel(log.New(&logs, "", 0), "info", ""), 1, 10)
	if err := s.EnableStorageSaver(t.TempDir(), "preset", false); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableOriginalFirst(q, "key"); err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		f, _ := mw.CreateFormFile("assetData", "source.jpg")
		f.Write([]byte("source"))
		f, _ = mw.CreateFormFile("sidecarData", "source.xmp")
		f.Write([]byte("sidecar"))
		mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("x-api-key", "key")
		w := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(w, r)
		return w
	}
	if result := request(); result.Code != http.StatusCreated || !bytes.Contains(result.Body.Bytes(), []byte(`"id":"real-id"`)) {
		t.Fatalf("first response: %d %s", result.Code, result.Body)
	}
	if !strings.Contains(logs.String(), "Optimization deferred") {
		t.Fatalf("missing deferred optimization log: %s", logs.String())
	}
	if result := request(); result.Code != http.StatusOK || !bytes.Contains(result.Body.Bytes(), []byte(`"id":"real-id"`)) {
		t.Fatalf("retry response: %d %s", result.Code, result.Body)
	}
	if uploadCount != 1 || job.OriginalAssetID != "real-id" {
		t.Fatalf("uploads=%d job=%+v", uploadCount, job)
	}
	if len(job.UploadFields[originalFirstMultipartField]) != 1 {
		t.Fatal("sidecar was not stored in job")
	}
	manifest, err := os.ReadFile(job.UploadFields[originalFirstMultipartField][0])
	if err != nil {
		t.Fatal(err)
	}
	var extras []originalFirstPart
	if err := json.Unmarshal(manifest, &extras); err != nil {
		t.Fatal(err)
	}
	if len(extras) != 1 {
		t.Fatalf("unexpected multipart extras: %#v", extras)
	}
	contents, err := os.ReadFile(extras[0].Path)
	if err != nil || string(contents) != "sidecar" {
		t.Fatalf("durable sidecar %q: %v", contents, err)
	}
}

func TestOriginalFirstUsesVerifiedLegacyAlias(t *testing.T) {
	original, optimized := []byte("legacy original"), []byte("optimized bytes")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			io.WriteString(w, `{"id":"owner"}`)
		case "/api/assets/legacy-id/original":
			w.Write(optimized)
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	s := NewServer(u, ":0", "/api/assets", "assetData", nil, []entity.Task{{Extensions: []string{"jpg"}}}, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
	if err := s.EnableStorageSaver(t.TempDir(), "preset", false); err != nil {
		t.Fatal(err)
	}
	err := s.StorageSaverStore().Put(state.Record{OwnerID: "owner", AssetID: "legacy-id", SourceSHA1: legacyDigest(original), OutputSHA1: legacyDigest(optimized), SourceFilename: "old.jpg", OutputFilename: "old.avif", Preset: "old", State: "verified", SourceSize: int64(len(original)), OutputSize: int64(len(optimized)), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	queue := OriginalFirstQueue{
		Enqueue: func(jobqueue.Input, io.Reader) (jobqueue.Job, bool, error) {
			t.Fatal("enqueued legacy alias")
			return jobqueue.Job{}, false, nil
		},
		FindIntent: func(string, string) (jobqueue.Job, bool, error) {
			t.Fatal("looked up queue for verified legacy alias")
			return jobqueue.Job{}, false, nil
		},
		MarkOriginalUploaded: func(string, string) error { t.Fatal("marked absent intent"); return nil },
	}
	if err := s.EnableOriginalFirst(queue, "key"); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	f, _ := mw.CreateFormFile("assetData", "old.jpg")
	_, _ = f.Write(original)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("x-api-key", "key")
	w := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"id":"legacy-id"`)) {
		t.Fatalf("response: %d %s", w.Code, w.Body)
	}
}

func TestOriginalFirstNativeDuplicateWithoutIntentIsNotEnqueued(t *testing.T) {
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
			json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"id": body.Assets[0].ID, "action": "reject", "reason": "duplicate", "assetId": "existing-id", "isTrashed": false}}})
		default:
			t.Errorf("unexpected upstream request %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	s := NewServer(u, ":0", "/api/assets", "assetData", nil, []entity.Task{{Extensions: []string{"jpg"}}}, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
	if err := s.EnableStorageSaver(t.TempDir(), "preset", false); err != nil {
		t.Fatal(err)
	}
	queue := OriginalFirstQueue{
		Enqueue: func(jobqueue.Input, io.Reader) (jobqueue.Job, bool, error) {
			t.Fatal("enqueued existing library asset")
			return jobqueue.Job{}, false, nil
		},
		FindIntent:           func(string, string) (jobqueue.Job, bool, error) { return jobqueue.Job{}, false, nil },
		MarkOriginalUploaded: func(string, string) error { t.Fatal("marked absent intent"); return nil },
	}
	if err := s.EnableOriginalFirst(queue, "key"); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	f, _ := mw.CreateFormFile("assetData", "old.jpg")
	_, _ = f.Write([]byte("old"))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("x-api-key", "key")
	w := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"id":"existing-id"`)) {
		t.Fatalf("response: %d %s", w.Code, w.Body)
	}
}

func TestOriginalFirstIntentSurvivesQueueReopen(t *testing.T) {
	var uploaded bool
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
			result := map[string]any{"id": body.Assets[0].ID, "action": "accept"}
			if uploaded {
				result = map[string]any{"id": body.Assets[0].ID, "action": "reject", "reason": "duplicate", "assetId": "original-id", "isTrashed": false}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{result}})
		case "/api/assets":
			uploaded = true
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"original-id"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	store, err := jobqueue.Open(filepath.Join(dir, "queue"), jobqueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	failMark := true
	queue := OriginalFirstQueue{
		Enqueue: func(in jobqueue.Input, reader io.Reader) (jobqueue.Job, bool, error) {
			return store.Enqueue(in, reader)
		},
		FindIntent: func(owner, hash string) (jobqueue.Job, bool, error) {
			for _, j := range store.List() {
				if j.OwnerID == owner && j.SourceSHA256 == hash {
					return j, true, nil
				}
			}
			return jobqueue.Job{}, false, nil
		},
		MarkOriginalUploaded: func(id, asset string) error {
			if failMark {
				failMark = false
				return errors.New("interrupted")
			}
			_, err := store.MarkOriginalUploaded(id, asset)
			return err
		},
	}
	u, _ := url.Parse(upstream.URL)
	s := NewServer(u, ":0", "/api/assets", "assetData", nil, []entity.Task{{Extensions: []string{"jpg"}, Command: "recipe"}}, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
	if err := s.EnableStorageSaver(dir, "preset", false); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableOriginalFirst(queue, "key"); err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		f, _ := mw.CreateFormFile("assetData", "photo.jpg")
		_, _ = f.Write([]byte("original"))
		_ = mw.Close()
		r := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("x-api-key", "key")
		w := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != http.StatusCreated || !bytes.Contains(w.Body.Bytes(), []byte(`"id":"original-id"`)) {
		t.Fatalf("first upload: %d %s", w.Code, w.Body)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = jobqueue.Open(filepath.Join(dir, "queue"), jobqueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if w := request(); w.Code != 200 {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	jobs := store.List()
	if len(jobs) != 1 || jobs[0].OriginalAssetID != "original-id" || jobs[0].Status != jobqueue.StatusQueued {
		t.Fatalf("recovered jobs: %#v", jobs)
	}
}
