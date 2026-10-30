package workerclient

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func hashes(b []byte) (string, string) {
	a := sha1.Sum(b)
	c := sha256.Sum256(b)
	return hex.EncodeToString(a[:]), hex.EncodeToString(c[:])
}

func TestDefaultHTTPTransportTimeouts(t *testing.T) {
	c, err := New(Config{ServerURL: "http://example.test", Token: "t", WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := c.cfg.HTTPClient.Transport.(*http.Transport)
	if !ok || transport == http.DefaultTransport {
		t.Fatal("expected cloned HTTP transport")
	}
	if c.cfg.HTTPClient.Timeout != 0 || transport.ResponseHeaderTimeout != 30*time.Second || transport.TLSHandshakeTimeout != 10*time.Second || transport.DialContext == nil {
		t.Fatalf("unexpected HTTP timeouts: client=%v header=%v TLS=%v", c.cfg.HTTPClient.Timeout, transport.ResponseHeaderTimeout, transport.TLSHandshakeTimeout)
	}
	injected := &http.Client{Timeout: time.Second}
	custom, err := New(Config{ServerURL: "http://example.test", Token: "t", WorkerID: "w", HTTPClient: injected})
	if err != nil || custom.cfg.HTTPClient != injected || injected.Transport != nil || injected.Timeout != time.Second {
		t.Fatalf("injected client changed: %v", err)
	}
}

func TestResponseHeaderTimeoutDoesNotLimitStreamingBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-headers" {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()
	c, err := New(Config{ServerURL: srv.URL, Token: "t", WorkerID: "w"})
	if err != nil {
		t.Fatal(err)
	}
	c.cfg.HTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout = 50 * time.Millisecond
	if _, err := c.request(context.Background(), "GET", "/slow-headers", nil, ""); err == nil {
		t.Fatal("expected response-header timeout")
	}
	resp, err := c.request(context.Background(), "GET", "/stream", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "body" {
		t.Fatalf("stream body=%q err=%v", body, err)
	}
}
func TestStreamingJobAndHeartbeat(t *testing.T) {
	source := []byte(strings.Repeat("jpeg", 100000))
	output := []byte("avif-output")
	s1, s2 := hashes(source)
	o1, o2 := hashes(output)
	var beats atomic.Int32
	var uploaded atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("X-Worker-ID") != "worker" {
			t.Error("missing authentication")
		}
		switch r.URL.Path {
		case "/internal/jobs/claim":
			var req map[string]string
			json.NewDecoder(r.Body).Decode(&req)
			if req["workerId"] != "worker" {
				t.Error(req)
			}
			json.NewEncoder(w).Encode(map[string]any{"id": "one", "filename": "photo.jpg", "preset": Preset, "sourceSha1": s1, "sourceSha256": s2, "size": len(source), "lease": map[string]string{"token": "lease"}})
		case "/internal/jobs/one/source":
			if r.Header.Get("X-Lease-Token") != "lease" {
				t.Error("source lease")
			}
			w.Write(source)
		case "/internal/jobs/one/heartbeat":
			if r.Header.Get("X-Lease-Token") != "lease" {
				t.Error("heartbeat lease")
			}
			beats.Add(1)
			w.WriteHeader(204)
		case "/internal/jobs/one/result":
			if r.Header.Get("X-Lease-Token") != "lease" || r.Header.Get("X-Output-Filename") != "photo.avif" || r.Header.Get("X-Output-SHA1") != o1 || r.Header.Get("X-Output-SHA256") != o2 || r.Header.Get("X-Output-Size") != fmt.Sprint(len(output)) {
				t.Error("bad output headers", r.Header)
			}
			b, _ := io.ReadAll(r.Body)
			if string(b) != string(output) {
				t.Error("bad output")
			}
			uploaded.Store(true)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	temp := t.TempDir()
	c, err := New(Config{ServerURL: srv.URL, Token: "secret", WorkerID: "worker", TempDir: temp, HeartbeatInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c.convert = func(ctx context.Context, _, src, dst string) (string, error) {
		b, err := os.ReadFile(src)
		if err != nil || string(b) != string(source) {
			t.Error("source mismatch", err)
		}
		time.Sleep(35 * time.Millisecond)
		return "", os.WriteFile(dst, output, 0600)
	}
	worked, err := c.RunOnce(context.Background())
	if err != nil || !worked || !uploaded.Load() || beats.Load() == 0 {
		t.Fatalf("worked=%v err=%v uploaded=%v beats=%d", worked, err, uploaded.Load(), beats.Load())
	}
	files, _ := os.ReadDir(temp)
	if len(files) != 0 {
		t.Fatal("temporary files not removed", files)
	}
}
func TestFailuresAndSkips(t *testing.T) {
	for _, tc := range []struct {
		name               string
		badDigest          bool
		converterMessage   string
		output             string
		skipped, retryable bool
	}{{"digest", true, "", "", false, true}, {"unsupported", false, "unsupported motion or HDR JPEG", "", true, false}, {"no gain", false, "", strings.Repeat("x", 10), true, false}, {"converter failure", false, "metadata validation failed", "", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			source := []byte("1234567890")
			s1, s2 := hashes(source)
			if tc.badDigest {
				s1 = strings.Repeat("0", 40)
			}
			var got failure
			var failCalled bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/jobs/claim":
					json.NewEncoder(w).Encode(Job{ID: "a", Filename: "a.jpg", Preset: Preset, SourceSHA1: s1, SourceSHA256: s2, SourceSize: int64(len(source)), LeaseToken: "l"})
				case "/internal/jobs/a/source":
					w.Write(source)
				case "/internal/jobs/a/fail":
					failCalled = true
					json.NewDecoder(r.Body).Decode(&got)
					w.WriteHeader(204)
				case "/internal/jobs/a/result":
					t.Error("unexpected upload")
				default:
					w.WriteHeader(204)
				}
			}))
			defer srv.Close()
			c, _ := New(Config{ServerURL: srv.URL, Token: "t", WorkerID: "w"})
			c.convert = func(_ context.Context, _, _, out string) (string, error) {
				if tc.converterMessage != "" {
					return tc.converterMessage, fmt.Errorf("exit 1")
				}
				return "", os.WriteFile(out, []byte(tc.output), 0600)
			}
			_, err := c.RunOnce(context.Background())
			if err != nil || !failCalled || got.Skipped != tc.skipped || got.Retryable != tc.retryable || got.Error == "" {
				t.Fatalf("err=%v called=%v failure=%+v", err, failCalled, got)
			}
		})
	}
}
func TestClaimFailureAndEmpty(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{ServerURL: srv.URL, Token: "t", WorkerID: "w"})
	if _, err := c.RunOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if worked, err := c.RunOnce(context.Background()); err != nil || worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
}
