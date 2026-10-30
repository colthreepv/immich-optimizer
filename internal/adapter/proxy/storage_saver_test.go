package proxy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/colthreepv/immich-optimizer/internal/adapter/state"
	"github.com/colthreepv/immich-optimizer/internal/domain/entity"
	customlogger "github.com/colthreepv/immich-optimizer/internal/logger"
)

func TestVerifiedIngestionRetryAndRestart(t *testing.T) {
	for _, failure := range []string{"none", "checksum", "metadata"} {
		t.Run(failure, func(t *testing.T) {
			var uploaded []byte
			var meta json.RawMessage
			uploads, converts := 0, 0
			sourceBytes := bytes.Repeat([]byte("x"), 100)
			h := sha1.Sum(sourceBytes)
			sourceDigest := hex.EncodeToString(h[:])
			fail := failure
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-api-key") != "valid" {
					w.WriteHeader(401)
					return
				}
				switch {
				case r.URL.Path == "/api/users/me":
					io.WriteString(w, `{"id":"owner"}`)
				case r.Method == "POST" && r.URL.Path == "/api/assets/bulk-upload-check":
					data, _ := io.ReadAll(r.Body)
					want := fmt.Sprintf(`{"assets":[{"id":%q,"checksum":%q}]}`, sourceDigest, sourceDigest)
					if string(data) != want {
						t.Errorf("bulk check body = %s, want %s", data, want)
					}
					if r.Header.Get("x-api-key") != "valid" {
						t.Error("bulk check did not forward user credentials")
					}
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, `{"results":[{"id":"`+sourceDigest+`","action":"accept"}]}`)
				case r.Method == "POST" && r.URL.Path == "/api/assets":
					if r.Header.Get("x-immich-checksum") != "" {
						t.Error("forwarded obsolete source checksum")
					}
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					defer r.MultipartForm.RemoveAll()
					f, h, e := r.FormFile("assetData")
					if e != nil {
						t.Error(e)
						return
					}
					defer f.Close()
					if h.Filename != "photo.avif" || h.Header.Get("Content-Type") != "image/avif" {
						t.Errorf("wrong output identity %s %s", h.Filename, h.Header.Get("Content-Type"))
					}
					if r.FormValue("fileCreatedAt") != "2020-01-02T03:04:05Z" {
						t.Error("lost date")
					}
					meta = json.RawMessage(r.FormValue("metadata"))
					uploaded, _ = io.ReadAll(f)
					uploads++
					w.WriteHeader(201)
					io.WriteString(w, `{"id":"asset","status":"created"}`)
				case r.URL.Path == "/api/assets/asset/original":
					if fail == "checksum" {
						io.WriteString(w, "corrupt")
					} else {
						w.Write(uploaded)
					}
				case r.Method == "PUT" && r.URL.Path == "/api/assets/asset/metadata":
					if fail == "metadata" {
						w.WriteHeader(500)
					} else {
						io.WriteString(w, "[]")
					}
				default:
					w.WriteHeader(404)
				}
			}))
			defer upstream.Close()
			u, _ := url.Parse(upstream.URL)
			dir := t.TempDir()
			proc := &mockProcessor{processFunc: func(ctx context.Context, path string, tasks []entity.Task) (*entity.ProcessResult, error) {
				converts++
				f, e := os.CreateTemp("", "converted-*.avif")
				if e != nil {
					return nil, e
				}
				f.WriteString("avif-bytes")
				f.Close()
				return &entity.ProcessResult{ProcessedFilePath: f.Name(), ProcessedFilename: "converted.avif", ProcessedSize: 10, OriginalSize: 100, Cleanup: func() { os.Remove(f.Name()) }}, nil
			}}
			makeServer := func() *Server {
				s := NewServer(u, ":0", "/api/assets", "assetData", proc, []entity.Task{{Extensions: []string{"jpg"}}}, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
				if e := s.EnableStorageSaver(dir, "preset", false); e != nil {
					t.Fatal(e)
				}
				return s
			}
			request := func(s *Server, key string) *httptest.ResponseRecorder {
				var b bytes.Buffer
				mw := multipart.NewWriter(&b)
				p, _ := mw.CreateFormFile("assetData", "photo.jpg")
				p.Write(sourceBytes)
				mw.WriteField("fileCreatedAt", "2020-01-02T03:04:05Z")
				mw.Close()
				r := httptest.NewRequest("POST", "/api/assets", &b)
				r.Header.Set("Content-Type", mw.FormDataContentType())
				r.Header.Set("x-api-key", key)
				r.Header.Set("x-immich-checksum", "source")
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				return w
			}
			s := makeServer()
			if w := request(s, "wrong"); w.Code != 401 || converts != 0 {
				t.Fatal("unauthenticated conversion")
			}
			first := request(s, "valid")
			if failure != "none" {
				if first.Code != 502 {
					t.Fatalf("false ACK: %d %s", first.Code, first.Body.String())
				}
				fail = ""
				s = makeServer()
				if w := request(s, "valid"); w.Code != 201 {
					t.Fatalf("retry: %d %s", w.Code, w.Body.String())
				}
			} else if first.Code != 201 {
				t.Fatalf("upload: %d %s", first.Code, first.Body.String())
			}
			if converts != 1 {
				t.Fatalf("reconverted durable output %d times", converts)
			}
			if !strings.Contains(string(meta), "sourceSHA1") {
				t.Fatal("missing reconstructable metadata")
			}
			before := uploads
			s = makeServer()
			if w := request(s, "valid"); w.Code != 200 || !strings.Contains(w.Body.String(), "duplicate") {
				t.Fatalf("retry duplicate: %d %s", w.Code, w.Body.String())
			}
			if uploads != before {
				t.Fatal("reuploaded verified asset")
			}
			ledger, e := state.Open(dir)
			if e != nil {
				t.Fatal(e)
			}
			rec, ok := ledger.FindSource("owner", hex.EncodeToString(h[:]))
			if !ok || rec.State != "verified" {
				t.Fatal("missing persisted verified alias")
			}
			if _, e = os.Stat(rec.SpoolPath); e != nil {
				t.Fatal("lost staged output")
			}
		})
	}
}
