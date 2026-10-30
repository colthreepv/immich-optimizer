package coordinator

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

func TestPausedWorkerQueueAndOperatorControls(t *testing.T) {
	root := t.TempDir()
	store, err := jobqueue.Open(root, jobqueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	source := []byte("source bytes")
	a, b := sha1.Sum(source), sha256.Sum256(source)
	job, _, err := store.Enqueue(jobqueue.Input{OwnerID: "owner", OriginalAssetID: "original", Filename: "source.jpg", Preset: "avif-q50-s6-420-v1", SourceSHA1: hex.EncodeToString(a[:]), SourceSHA256: hex.EncodeToString(b[:]), Size: int64(len(source))}, bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	handler := &WorkerHandler{Queue: store, Token: "secret", Paused: true}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("X-Worker-ID", "operator")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := call("POST", "/internal/jobs/claim", `{"workerId":"operator"}`); got.Code != http.StatusNoContent {
		t.Fatalf("paused claim %d", got.Code)
	}
	current, _ := store.Get(job.ID)
	if current.Status != jobqueue.StatusQueued {
		t.Fatalf("paused queue changed: %+v", current)
	}
	response := call("GET", "/internal/jobs", "")
	if response.Code != 200 || strings.Contains(response.Body.String(), root) || strings.Contains(response.Body.String(), "sourceSpoolPath") {
		t.Fatal("operator list leaks paths or fails", response.Body.String())
	}
	var listed struct {
		Jobs []struct {
			ID string `json:"id"`
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Jobs) != 1 || listed.Jobs[0].ID != job.ID {
		t.Fatal("invalid job list", err)
	}
	leased, err := store.Claim("operator")
	if err != nil {
		t.Fatal(err)
	}
	output := []byte("AVIF")
	x, y := sha1.Sum(output), sha256.Sum256(output)
	_, err = store.Complete(job.ID, "operator", leased.Lease.Token, jobqueue.ResultInput{SHA1: hex.EncodeToString(x[:]), SHA256: hex.EncodeToString(y[:]), Size: int64(len(output)), Filename: "output.avif"}, bytes.NewReader(output))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.HoldPublication(job.ID, "operator review"); err != nil {
		t.Fatal(err)
	}
	response = call("POST", "/internal/jobs/"+job.ID+"/resume", "")
	if response.Code != 200 {
		t.Fatal("resume failed", response.Body.String())
	}
	current, _ = store.Get(job.ID)
	if current.Status != jobqueue.StatusPublishing {
		t.Fatal("resume did not restore publication")
	}
	response = call("POST", "/internal/jobs/"+job.ID+"/resume", "")
	if response.Code != http.StatusConflict {
		t.Fatal("non-held job resumed")
	}
}
