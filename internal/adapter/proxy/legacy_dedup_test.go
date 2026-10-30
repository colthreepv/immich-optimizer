package proxy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
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

const legacyAPIKey = "legacy-user-key"

func legacyDigest(data []byte) string {
	digest := sha1.Sum(data)
	return hex.EncodeToString(digest[:])
}

func legacyBulkBody(digest string) string {
	return `{"assets":[{"id":"` + digest + `","checksum":"` + digest + `"}]}`
}

func legacyUploadRequest(t *testing.T, server *Server, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("assetData", "legacy.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("x-api-key", legacyAPIKey)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

func legacyStorageServer(t *testing.T, upstream *url.URL, dir string, processor *mockProcessor) *Server {
	t.Helper()
	server := NewServer(upstream, ":0", "/api/assets", "assetData", processor, []entity.Task{{Extensions: []string{"jpg"}}}, customlogger.NewWithLevel(nil, "error", ""), 1, 10)
	if err := server.EnableStorageSaver(dir, "test-preset", false); err != nil {
		t.Fatal(err)
	}
	return server
}

func TestLegacyJPEGDuplicateUsesOriginalIdentity(t *testing.T) {
	source := bytes.Repeat([]byte("stock-mobile-jpeg"), 32)
	digest := legacyDigest(source)
	checks, converts, uploads, metadataUpdates, downloads := 0, 0, 0, 0, 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != legacyAPIKey {
			t.Error("upstream request lost user credentials")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/users/me":
			io.WriteString(w, `{"id":"legacy-owner"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check":
			checks++
			body, _ := io.ReadAll(r.Body)
			if string(body) != legacyBulkBody(digest) {
				t.Errorf("bulk check body = %s, want %s", body, legacyBulkBody(digest))
			}
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("bulk check content type = %q", r.Header.Get("Content-Type"))
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"results":[{"id":"`+digest+`","action":"reject","reason":"duplicate","assetId":"legacy-asset","isTrashed":false}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/legacy-asset/original":
			downloads++
			w.Write(source)
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
			uploads++
			t.Error("uploaded a duplicate JPEG")
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPut && r.URL.Path == "/api/assets/legacy-asset/metadata":
			metadataUpdates++
			t.Error("changed metadata on an existing asset")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	dir := t.TempDir()
	processor := &mockProcessor{processFunc: func(context.Context, string, []entity.Task) (*entity.ProcessResult, error) {
		converts++
		t.Error("converted an existing JPEG")
		return nil, nil
	}}
	u, _ := url.Parse(upstream.URL)
	recorder := legacyUploadRequest(t, legacyStorageServer(t, u, dir, processor), source)
	if recorder.Code != http.StatusOK {
		t.Fatalf("duplicate response: %d %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["id"] != "legacy-asset" || response["status"] != "duplicate" {
		t.Fatalf("duplicate response = %#v", response)
	}
	if checks != 1 || converts != 0 || uploads != 0 || metadataUpdates != 0 || downloads != 1 {
		t.Fatalf("checks=%d converts=%d uploads=%d metadata=%d downloads=%d", checks, converts, uploads, metadataUpdates, downloads)
	}

	ledger, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, found := ledger.FindSource("legacy-owner", digest)
	if !found {
		t.Fatal("missing identity record")
	}
	if record.AssetID != "legacy-asset" || record.State != "verified" || record.SourceSHA1 != digest || record.OutputSHA1 != digest {
		t.Fatalf("identity record = %#v", record)
	}
	if record.SourceSize != int64(len(source)) || record.OutputSize != int64(len(source)) {
		t.Fatalf("identity sizes = %d/%d, want %d", record.SourceSize, record.OutputSize, len(source))
	}
}

func TestLegacyJPEGNewUploadConvertsAndVerifies(t *testing.T) {
	source := bytes.Repeat([]byte("new-jpeg"), 40)
	digest := legacyDigest(source)
	output := []byte("small-avif-output")
	preflightSeen := false
	converts, uploads, metadataUpdates := 0, 0, 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != legacyAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/users/me":
			io.WriteString(w, `{"id":"legacy-owner"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check":
			preflightSeen = true
			body, _ := io.ReadAll(r.Body)
			if string(body) != legacyBulkBody(digest) {
				t.Errorf("bulk check body = %s, want %s", body, legacyBulkBody(digest))
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"results":[{"id":"`+digest+`","action":"accept"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
			uploads++
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"new-asset","status":"created"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/assets/new-asset/original":
			w.Write(output)
		case r.Method == http.MethodPut && r.URL.Path == "/api/assets/new-asset/metadata":
			metadataUpdates++
			io.WriteString(w, `[]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	dir := t.TempDir()
	processor := &mockProcessor{processFunc: func(context.Context, string, []entity.Task) (*entity.ProcessResult, error) {
		converts++
		if !preflightSeen {
			t.Error("conversion started before original checksum preflight")
		}
		file, err := os.CreateTemp("", "legacy-output-*.avif")
		if err != nil {
			return nil, err
		}
		if _, err = file.Write(output); err != nil {
			file.Close()
			return nil, err
		}
		if err = file.Close(); err != nil {
			return nil, err
		}
		return &entity.ProcessResult{
			ProcessedFilePath: file.Name(),
			ProcessedFilename: "legacy.avif",
			ProcessedSize:     int64(len(output)),
			OriginalSize:      int64(len(source)),
			Cleanup:           func() { os.Remove(file.Name()) },
		}, nil
	}}
	u, _ := url.Parse(upstream.URL)
	recorder := legacyUploadRequest(t, legacyStorageServer(t, u, dir, processor), source)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("new upload response: %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"id":"new-asset"`) {
		t.Fatalf("lost upstream upload response: %s", recorder.Body.String())
	}
	if converts != 1 || uploads != 1 || metadataUpdates != 1 {
		t.Fatalf("converts=%d uploads=%d metadata=%d", converts, uploads, metadataUpdates)
	}
}

func TestLegacyJPEGPreflightFailsClosed(t *testing.T) {
	source := bytes.Repeat([]byte("preflight-jpeg"), 32)
	digest := legacyDigest(source)
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		wantBody   string
	}{
		{name: "upstream error", status: http.StatusInternalServerError, body: `{}`, wantStatus: http.StatusBadGateway},
		{name: "invalid JSON", status: http.StatusOK, body: `{`, wantStatus: http.StatusBadGateway},
		{name: "missing results", status: http.StatusOK, body: `{}`, wantStatus: http.StatusBadGateway},
		{name: "missing result", status: http.StatusOK, body: `{"results":[]}`, wantStatus: http.StatusBadGateway},
		{name: "unrelated result", status: http.StatusOK, body: `{"results":[{"id":"another-file","action":"accept"}]}`, wantStatus: http.StatusBadGateway},
		{name: "unsupported rejection", status: http.StatusOK, body: `{"results":[{"id":"` + digest + `","action":"reject","reason":"unsupported-format"}]}`, wantStatus: http.StatusBadGateway},
		{name: "trashed duplicate", status: http.StatusOK, body: `{"results":[{"id":"` + digest + `","action":"reject","reason":"duplicate","assetId":"trashed-asset","isTrashed":true}]}`, wantStatus: http.StatusConflict, wantBody: "trash"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converts, uploads, downloads, metadataUpdates := 0, 0, 0, 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("x-api-key") != legacyAPIKey {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch {
				case r.URL.Path == "/api/users/me":
					io.WriteString(w, `{"id":"legacy-owner"}`)
				case r.Method == http.MethodPost && r.URL.Path == "/api/assets/bulk-upload-check":
					body, _ := io.ReadAll(r.Body)
					if string(body) != legacyBulkBody(digest) {
						t.Errorf("bulk check body = %s, want %s", body, legacyBulkBody(digest))
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.status)
					io.WriteString(w, test.body)
				case r.Method == http.MethodGet && r.URL.Path == "/api/assets/trashed-asset/original":
					downloads++
					w.Write(source)
				case r.Method == http.MethodPost && r.URL.Path == "/api/assets":
					uploads++
					w.WriteHeader(http.StatusInternalServerError)
				case r.Method == http.MethodPut && r.URL.Path == "/api/assets/trashed-asset/metadata":
					metadataUpdates++
					w.WriteHeader(http.StatusInternalServerError)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()

			dir := t.TempDir()
			processor := &mockProcessor{processFunc: func(context.Context, string, []entity.Task) (*entity.ProcessResult, error) {
				converts++
				return nil, nil
			}}
			u, _ := url.Parse(upstream.URL)
			recorder := legacyUploadRequest(t, legacyStorageServer(t, u, dir, processor), source)
			if recorder.Code != test.wantStatus {
				t.Fatalf("response = %d %s, want %d", recorder.Code, recorder.Body.String(), test.wantStatus)
			}
			if test.wantBody != "" && !strings.Contains(strings.ToLower(recorder.Body.String()), test.wantBody) {
				t.Fatalf("response lacks %q: %s", test.wantBody, recorder.Body.String())
			}
			if converts != 0 || uploads != 0 || downloads != 0 || metadataUpdates != 0 {
				t.Fatalf("converts=%d uploads=%d downloads=%d metadata=%d", converts, uploads, downloads, metadataUpdates)
			}
			ledger, err := state.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, found := ledger.FindSource("legacy-owner", digest); found {
				t.Fatal("stored a record after failed preflight")
			}
		})
	}
}
