package coordinator

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colthreepv/immich-optimizer/internal/adapter/workerclient"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

func testHashes(data []byte) (string, string) {
	a := sha1.Sum(data)
	b := sha256.Sum256(data)
	return hex.EncodeToString(a[:]), hex.EncodeToString(b[:])
}

func TestActualWorkerHeartbeatIntegration(t *testing.T) {
	store, err := jobqueue.Open(t.TempDir(), jobqueue.Options{LeaseDuration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := []byte("original jpeg data long enough to allow a smaller output")
	h1, h256 := testHashes(source)
	job, _, err := store.Enqueue(jobqueue.Input{OriginalAssetID: "original", OwnerID: "owner", Filename: "photo.jpg", Preset: workerclient.Preset, Size: int64(len(source)), SourceSHA1: h1, SourceSHA256: h256}, bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	var heartbeats atomic.Int32
	handler := &WorkerHandler{Queue: store, Token: "secret"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code == http.StatusOK {
				heartbeats.Add(1)
			}
			for k, v := range response.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	dir := t.TempDir()
	program := filepath.Join(dir, "converter.go")
	executable := filepath.Join(dir, "converter")
	if runtime.GOOS == "windows" {
		executable += ".exe"
	}
	code := `package main
import ("os"; "time")
func main() { time.Sleep(250*time.Millisecond); if err := os.WriteFile(os.Args[2], []byte("avif"), 0600); err != nil { panic(err) } }`
	if err := os.WriteFile(program, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", executable, program).CombinedOutput(); err != nil {
		t.Fatalf("build converter: %v: %s", err, out)
	}
	client, err := workerclient.New(workerclient.Config{ServerURL: server.URL, Token: "secret", WorkerID: "worker", Converter: executable, HeartbeatInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	worked, err := client.RunOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("worker: worked=%v err=%v", worked, err)
	}
	if heartbeats.Load() == 0 {
		t.Fatal("no worker heartbeat was accepted by the real handler")
	}
	updated, err := store.Get(job.ID)
	if err != nil || updated.Status != jobqueue.StatusPublishing {
		t.Fatalf("job: %+v, %v", updated, err)
	}
}
func TestWorkerHTTPIntegration(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store, err := jobqueue.Open(t.TempDir(), jobqueue.Options{Now: func() time.Time { return now }, LeaseDuration: time.Minute, RetryBase: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := &WorkerHandler{Queue: store, Token: "secret"}
	source := []byte("original jpeg content")
	sh1, sh256 := testHashes(source)
	enqueue := func(asset string) jobqueue.Job {
		j, _, err := store.Enqueue(jobqueue.Input{OriginalAssetID: asset, OwnerID: asset, Filename: "photo.jpg", Preset: "avif-q50-s6-420-v1", Size: int64(len(source)), SourceSHA1: sh1, SourceSHA256: sh256}, bytes.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	call := func(method, path, worker, lease string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		if headers["auth"] != "none" {
			r.Header.Set("Authorization", "Bearer secret")
		}
		if worker != "" {
			r.Header.Set("X-Worker-ID", worker)
		}
		if lease != "" {
			r.Header.Set("X-Lease-Token", lease)
		}
		for k, v := range headers {
			if k != "auth" {
				r.Header.Set(k, v)
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	claim := func(worker string) (string, string) {
		t.Helper()
		w := call("POST", "/internal/jobs/claim", worker, "", []byte(fmt.Sprintf(`{"workerId":%q}`, worker)), nil)
		if w.Code != 200 {
			t.Fatalf("claim %d %s", w.Code, w.Body.String())
		}
		var job struct {
			ID    string `json:"id"`
			Token string `json:"leaseToken"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		return job.ID, job.Token
	}
	enqueue("original-1")
	if w := call("POST", "/internal/jobs/claim", "worker", "", []byte(`{"workerId":"worker"}`), map[string]string{"auth": "none"}); w.Code != 401 {
		t.Fatalf("missing auth: %d", w.Code)
	}
	if w := call("GET", "/internal/jobs/no-such-job/source", "worker", "lease", nil, map[string]string{"Authorization": "secret"}); w.Code != 401 {
		t.Errorf("missing Bearer scheme accepted: %d", w.Code)
	}
	if w := call("POST", "/internal/jobs/claim", "worker", "", []byte(`{"workerId":"other"}`), nil); w.Code != 400 {
		t.Fatalf("worker mismatch: %d", w.Code)
	}
	id, token := claim("worker")
	path := "/internal/jobs/" + id + "/"
	if w := call("POST", path+"heartbeat", "worker", token, nil, nil); w.Code != 200 {
		t.Errorf("worker heartbeat without body: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", path+"source", "other", token, nil, nil); w.Code != 409 {
		t.Fatalf("wrong worker: %d", w.Code)
	}
	if w := call("GET", path+"source", "worker", "wrong", nil, nil); w.Code != 409 {
		t.Fatalf("wrong lease: %d", w.Code)
	}
	if w := call("GET", path+"source", "worker", token, nil, nil); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), source) {
		t.Fatalf("source %d %s", w.Code, w.Body.String())
	}
	output := []byte("avif")
	h1, h256 := testHashes(output)
	headers := map[string]string{"X-Output-Filename": "photo.avif", "X-Output-Size": fmt.Sprint(len(output)), "X-Output-SHA1": h1, "X-Output-SHA256": h256}
	bad := map[string]string{}
	for k, v := range headers {
		bad[k] = v
	}
	bad["X-Output-SHA256"] = strings.Repeat("0", 64)
	if w := call("PUT", path+"result", "worker", token, output, bad); w.Code < 400 {
		t.Fatalf("bad digest accepted: %d", w.Code)
	}
	if w := call("PUT", path+"result", "worker", token, output, headers); w.Code != 200 {
		t.Fatalf("result: %d %s", w.Code, w.Body.String())
	}
	if w := call("PUT", path+"result", "worker", token, output, headers); w.Code != 200 {
		t.Fatalf("idempotent result: %d %s", w.Code, w.Body.String())
	}
	j, err := store.Get(id)
	if err != nil || j.Status != jobqueue.StatusPublishing {
		t.Fatalf("result state %+v %v", j, err)
	}
	got, err := os.ReadFile(j.ResultPath)
	if err != nil || !bytes.Equal(got, output) {
		t.Fatal("stored result mismatch", err)
	}
	enqueue("original-2")
	id, token = claim("worker")
	now = now.Add(2 * time.Minute)
	if w := call("PUT", "/internal/jobs/"+id+"/result", "worker", token, output, headers); w.Code != 409 {
		t.Fatalf("stale result: %d %s", w.Code, w.Body.String())
	}
	now = now.Add(2 * time.Second)
	_, _ = store.Claim("other") // expire the lease and schedule backoff
	now = now.Add(2 * time.Second)
	id, token = claim("worker")
	fail := []byte(fmt.Sprintf(`{"workerId":"worker","token":%q,"error":"no gain","skipped":true,"retryable":false}`, token))
	if w := call("POST", "/internal/jobs/"+id+"/fail", "worker", token, fail, nil); w.Code != 200 {
		t.Fatalf("skip: %d %s", w.Code, w.Body.String())
	}
	j, err = store.Get(id)
	if err != nil || j.Status != jobqueue.StatusSkipped {
		t.Fatalf("skip state %+v %v", j, err)
	}
}
