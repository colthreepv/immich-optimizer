package proxy

import (
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/colthreepv/immich-optimizer/internal/adapter/state"
	customlogger "github.com/colthreepv/immich-optimizer/internal/logger"
)

func TestMobileProtocolIntegration(t *testing.T) {
	a := strings.Repeat("ab", 20)
	b := strings.Repeat("cd", 20)
	bBytes, _ := hex.DecodeString(b)
	aBytes, _ := hex.DecodeString(a)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/users/me":
			io.WriteString(w, `{"id":"owner"}`)
		case "/api/sync/stream":
			w.Header().Set("Content-Type", "application/jsonlines+json")
			io.WriteString(w, `{"type":"AssetV2","data":{"id":"asset","ownerId":"owner","checksum":"`+base64.StdEncoding.EncodeToString(bBytes)+`"},"ack":"checkpoint"}`+"\n")
		case "/api/assets/bulk-upload-check":
			data, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(data), b) {
				t.Errorf("preflight not translated: %s", data)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"results":[{"id":"phone","action":"reject","reason":"duplicate","assetId":"asset","isTrashed":false}]}`)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	s := NewServer(u, ":0", "/api/assets", "assetData", nil, nil, customlogger.NewWithLevel(nil, "error", ""), 4, 10)
	if err := s.EnableStorageSaver(t.TempDir(), "test", true); err != nil {
		t.Fatal(err)
	}
	if err := s.saver.store.Put(state.Record{OwnerID: "owner", AssetID: "asset", SourceSHA1: a, OutputSHA1: b, State: "verified"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/sync/stream", "/api/assets/bulk-upload-check"} {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"assets":[{"id":"phone","checksum":"`+a+`"}]}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-api-key", "key")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if path == "/api/sync/stream" && !strings.Contains(w.Body.String(), base64.StdEncoding.EncodeToString(aBytes)) {
			t.Fatalf("sync not translated: %s", w.Body.String())
		}
		if path == "/api/assets/bulk-upload-check" && !strings.Contains(w.Body.String(), `"id":"phone"`) {
			t.Fatal("lost client ID")
		}
	}
}
