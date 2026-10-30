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

	"github.com/colthreepv/immich-optimizer/internal/domain/entity"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
	customlogger "github.com/colthreepv/immich-optimizer/internal/logger"
)

func TestOriginalFirstAdmissionFailureStillUploadsOriginal(t *testing.T) {
	tests := []struct {
		name                                            string
		lookupErr, enqueueErr                           error
		mismatch, manifestFail, upstreamFail, duplicate bool
		expected                                        int
	}{
		{name: "queue full", enqueueErr: jobqueue.ErrLimit, expected: 201},
		{name: "queue closed at lookup", lookupErr: jobqueue.ErrClosed, expected: 201},
		{name: "queue closed with native duplicate", lookupErr: jobqueue.ErrClosed, duplicate: true, expected: 200},
		{name: "queue closed at admission", enqueueErr: jobqueue.ErrClosed, expected: 201},
		{name: "background owner mismatch", mismatch: true, expected: 201},
		{name: "manifest persistence fails", manifestFail: true, expected: 201},
		{name: "upstream upload fails", enqueueErr: jobqueue.ErrLimit, upstreamFail: true, expected: 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls, marks, conversions, uploads int
			var manifest string
			var logs bytes.Buffer
			queue := OriginalFirstQueue{
				FindIntent: func(string, string) (jobqueue.Job, bool, error) { return jobqueue.Job{}, false, tt.lookupErr },
				Enqueue: func(in jobqueue.Input, _ io.Reader) (jobqueue.Job, bool, error) {
					calls++
					manifest = in.UploadFields[originalFirstMultipartField][0]
					return jobqueue.Job{}, false, tt.enqueueErr
				},
				MarkOriginalUploaded: func(string, string) error { marks++; return nil },
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/users/me":
					if tt.mismatch && r.Header.Get("x-api-key") == "service" {
						io.WriteString(w, `{"id":"other"}`)
					} else {
						io.WriteString(w, `{"id":"owner"}`)
					}
				case "/api/assets/bulk-upload-check":
					var body struct {
						Assets []struct {
							ID string `json:"id"`
						} `json:"assets"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					result := map[string]any{"id": body.Assets[0].ID, "action": "accept"}
					if tt.duplicate {
						result = map[string]any{"id": body.Assets[0].ID, "action": "reject", "reason": "duplicate", "assetId": "existing-real-id", "isTrashed": false}
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{result}})
				case "/api/assets":
					uploads++
					if r.Header.Get("x-api-key") != "client" {
						t.Error("upload did not use client credentials")
					}
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatal(err)
					}
					f, _, err := r.FormFile("assetData")
					if err != nil {
						t.Fatal(err)
					}
					b, _ := io.ReadAll(f)
					f.Close()
					if string(b) != "original" {
						t.Errorf("source = %q", b)
					}
					if r.FormValue("deviceAssetId") != "device-id" {
						t.Error("metadata not forwarded")
					}
					f, _, err = r.FormFile("sidecarData")
					if err != nil {
						t.Fatal(err)
					}
					b, _ = io.ReadAll(f)
					f.Close()
					if string(b) != "sidecar" {
						t.Errorf("sidecar = %q", b)
					}
					if tt.upstreamFail {
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(201)
					io.WriteString(w, `{"id":"actual-immich-id","status":"created"}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			u, _ := url.Parse(upstream.URL)
			processor := &mockProcessor{processFunc: func(context.Context, string, []entity.Task) (*entity.ProcessResult, error) {
				conversions++
				return nil, nil
			}}
			s := NewServer(u, ":0", "/api/assets", "assetData", processor, []entity.Task{{Extensions: []string{"jpg"}, Command: "recipe"}}, customlogger.NewWithLevel(log.New(&logs, "", 0), "info", ""), 1, 10)
			dir := t.TempDir()
			if err := s.EnableStorageSaver(dir, "preset", false); err != nil {
				t.Fatal(err)
			}
			if tt.lookupErr != nil {
				store, err := jobqueue.Open(filepath.Join(dir, "queue"), jobqueue.Options{})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				_, _, err = store.FindIntent("owner", strings.Repeat("0", 64))
				if !errors.Is(err, jobqueue.ErrClosed) {
					t.Fatalf("closed queue lookup: %v", err)
				}
				queue.FindIntent = store.FindIntent
			}
			if tt.manifestFail {
				if err := os.WriteFile(filepath.Join(dir, "original-first-parts"), []byte("obstacle"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.EnableOriginalFirst(queue, "service"); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			_ = mw.WriteField("deviceAssetId", "device-id")
			f, _ := mw.CreateFormFile("assetData", "photo.jpg")
			_, _ = f.Write([]byte("original"))
			f, _ = mw.CreateFormFile("sidecarData", "photo.xmp")
			_, _ = f.Write([]byte("sidecar"))
			_ = mw.Close()
			r := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
			r.Header.Set("Content-Type", mw.FormDataContentType())
			r.Header.Set("x-api-key", "client")
			w := httptest.NewRecorder()
			s.server.Handler.ServeHTTP(w, r)
			if w.Code != tt.expected {
				t.Fatalf("response: %d %s", w.Code, w.Body)
			}
			if tt.expected == 201 && !bytes.Contains(w.Body.Bytes(), []byte(`"id":"actual-immich-id"`)) {
				t.Fatalf("response: %s", w.Body)
			}
			if tt.duplicate && (uploads != 0 || !bytes.Contains(w.Body.Bytes(), []byte(`"id":"existing-real-id"`))) {
				t.Fatalf("duplicate response: uploads=%d body=%s", uploads, w.Body)
			}
			if marks != 0 || conversions != 0 {
				t.Fatalf("marks=%d conversions=%d", marks, conversions)
			}
			if !strings.Contains(logs.String(), "Optimization missed") {
				t.Fatalf("missing admission log: %s", logs.String())
			}
			if (tt.lookupErr != nil || tt.mismatch || tt.manifestFail) && calls != 0 {
				t.Fatalf("unexpected queue admission calls %d", calls)
			}
			if tt.enqueueErr != nil {
				if calls != 1 {
					t.Fatalf("queue admission calls %d", calls)
				}
				if _, err := os.Stat(manifest); err != nil {
					t.Fatalf("uncertain admission lost manifest: %v", err)
				}
			}
		})
	}
}

func TestOriginalFirstStaleJobUsesCurrentNativeAsset(t *testing.T) {
	var uploaded, trashed bool
	var uploads, conversions int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			_, _ = io.WriteString(w, `{"id":"owner"}`)
		case "/api/assets/bulk-upload-check":
			var body struct {
				Assets []struct {
					ID string `json:"id"`
				} `json:"assets"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			result := map[string]any{"id": body.Assets[0].ID, "action": "accept"}
			if uploaded {
				result = map[string]any{"id": body.Assets[0].ID, "action": "reject", "reason": "duplicate", "assetId": "current-id", "isTrashed": trashed}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{result}})
		case "/api/assets":
			uploads++
			if r.Header.Get("x-api-key") != "client" {
				t.Error("upload did not retain client credentials")
			}
			uploaded = true
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":"current-id","status":"created"}`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()
	queue := OriginalFirstQueue{
		FindIntent: func(string, string) (jobqueue.Job, bool, error) {
			return jobqueue.Job{ID: "old-job", Input: jobqueue.Input{OriginalAssetID: "purged-id"}}, true, nil
		},
		Enqueue: func(jobqueue.Input, io.Reader) (jobqueue.Job, bool, error) {
			t.Error("stale job was re-enqueued")
			return jobqueue.Job{}, false, nil
		},
		MarkOriginalUploaded: func(string, string) error { t.Error("stale job was retargeted"); return nil },
	}
	var logs bytes.Buffer
	u, _ := url.Parse(upstream.URL)
	processor := &mockProcessor{processFunc: func(context.Context, string, []entity.Task) (*entity.ProcessResult, error) {
		conversions++
		return nil, nil
	}}
	s := NewServer(u, ":0", "/api/assets", "assetData", processor, []entity.Task{{Extensions: []string{"jpg"}, Command: "recipe"}}, customlogger.NewWithLevel(log.New(&logs, "", 0), "info", ""), 1, 10)
	if err := s.EnableStorageSaver(t.TempDir(), "preset", false); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableOriginalFirst(queue, "service"); err != nil {
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
		r.Header.Set("x-api-key", "client")
		w := httptest.NewRecorder()
		s.server.Handler.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != 201 || !strings.Contains(w.Body.String(), `"id":"current-id"`) {
		t.Fatalf("new upload: %d %s", w.Code, w.Body)
	}
	if w := request(); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"current-id"`) {
		t.Fatalf("current duplicate: %d %s", w.Code, w.Body)
	}
	trashed = true
	if w := request(); w.Code != 409 {
		t.Fatalf("trashed duplicate: %d %s", w.Code, w.Body)
	}
	if uploads != 1 || conversions != 0 {
		t.Fatalf("uploads=%d conversions=%d", uploads, conversions)
	}
	if strings.Count(logs.String(), "Optimization missed") != 2 {
		t.Fatalf("missing stale-job logs: %s", logs.String())
	}
}
