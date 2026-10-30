package coordinator

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colthreepv/immich-optimizer/internal/adapter/replacement"
	"github.com/colthreepv/immich-optimizer/internal/adapter/state"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

// This fixture uses the production queue, ledger, and replacement HTTP client.
// Only codec validation is injected; real codec checks have separate fixtures.
type publisherFixture struct {
	t                                  *testing.T
	mu                                 sync.Mutex
	queue                              *jobqueue.Store
	ledger                             *state.Store
	queueDir, ledgerDir                string
	job                                jobqueue.Job
	source, output                     []byte
	publisher                          *Publisher
	requests                           []string
	uploads, validations               int
	republishAttempts                  int
	uploaded, copied, updated, trashed bool
	outputPending                      bool
	failRepublish                      bool
	sharedPreviewID                    string
	omitSharedOutput                   bool
	targetMetadata                     map[string]map[string]any
}

func publisherDigests(data []byte) (string, string) {
	a, b := sha1.Sum(data), sha256.Sum256(data)
	return hex.EncodeToString(a[:]), hex.EncodeToString(b[:])
}

func newPublisherFixture(t *testing.T) *publisherFixture {
	t.Helper()
	dir := t.TempDir()
	f := &publisherFixture{t: t, queueDir: filepath.Join(dir, "queue"), ledgerDir: filepath.Join(dir, "ledger"), source: []byte("original jpeg bytes are larger than the optimized output"), output: []byte("optimized avif"), targetMetadata: map[string]map[string]any{}}
	var err error
	f.queue, err = jobqueue.Open(f.queueDir, jobqueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.queue.Close(); err != nil {
			t.Error(err)
		}
	})
	f.ledger, err = state.Open(f.ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	h1, h256 := publisherDigests(f.source)
	f.job, _, err = f.queue.Enqueue(jobqueue.Input{OwnerID: "owner", OriginalAssetID: "original", SourceSHA1: h1, SourceSHA256: h256, Size: int64(len(f.source)), Filename: "source.jpg", Preset: "balanced", UploadFields: map[string][]string{"isFavorite": {"true"}}}, bytes.NewReader(f.source))
	if err != nil {
		t.Fatal(err)
	}
	f.job, err = f.queue.Claim("worker")
	if err != nil {
		t.Fatal(err)
	}
	h1, h256 = publisherDigests(f.output)
	f.job, err = f.queue.Complete(f.job.ID, "worker", f.job.Lease.Token, jobqueue.ResultInput{SHA1: h1, SHA256: h256, Size: int64(len(f.output)), Filename: "result.avif"}, bytes.NewReader(f.output))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.publisher = &Publisher{Queue: f.queue, Ledger: f.ledger, Client: replacement.NewClient(srv.URL, "test-key", srv.Client()), RetireOriginal: true, VerifyAVIF: func(_ context.Context, sourcePath, resultPath string) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if sourcePath != f.job.SourceSpoolPath || resultPath != f.job.ResultPath {
			return fmt.Errorf("validator received wrong source/result paths")
		}
		f.validations++
		return nil
	}}
	return f
}

func (f *publisherFixture) provenance() map[string]any {
	return map[string]any{"schemaVersion": 1, "ownerId": f.job.OwnerID, "sourceSHA1": f.job.SourceSHA1, "outputSHA1": f.job.OutputSHA1, "sourceFilename": f.job.Filename, "outputFilename": f.job.OutputFilename, "preset": f.job.Preset, "sourceSize": f.job.Size, "outputSize": f.job.OutputSize}
}

func (f *publisherFixture) asset(id, sha string) map[string]any {
	raw, err := hex.DecodeString(sha)
	if err != nil {
		f.t.Error(err)
	}
	size := f.job.OutputSize
	if id == "original" {
		size = f.job.Size
	}
	a := map[string]any{"id": id, "ownerId": f.job.OwnerID, "checksum": base64.StdEncoding.EncodeToString(raw), "type": "IMAGE", "visibility": "timeline", "width": 1024, "height": 768, "isFavorite": true, "isTrashed": false, "isOffline": false, "fileCreatedAt": "2024-01-01T00:00:00Z", "fileModifiedAt": "2024-01-02T00:00:00Z", "exifInfo": map[string]any{"description": "family photo", "fileSizeInByte": size}}
	if id == "original" {
		a["isTrashed"] = f.trashed
	} else if !f.updated {
		a["isFavorite"] = false
		a["exifInfo"] = map[string]any{"fileSizeInByte": size}
	}
	if id == "output" && f.outputPending {
		a["exifInfo"] = map[string]any{}
		a["width"] = nil
	}
	return a
}

func (f *publisherFixture) assertStage(want jobqueue.PublicationStage) {
	j, err := f.queue.Get(f.job.ID)
	if err != nil || j.PublicationStage != want {
		f.t.Errorf("external action at checkpoint %q, want %q (error %v)", j.PublicationStage, want, err)
	}
}

func (f *publisherFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("x-api-key") != "test-key" {
		f.t.Error("missing API key")
	}
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Error(err)
		}
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/users/me":
		write(map[string]any{"id": f.job.OwnerID})
	case "GET /api/assets/original":
		write(f.asset("original", f.job.SourceSHA1))
	case "GET /api/assets/output":
		write(f.asset("output", f.job.OutputSHA1))
	case "GET /api/assets/original/original":
		_, _ = w.Write(f.source)
	case "GET /api/assets/output/original":
		_, _ = w.Write(f.output)
	case "GET /api/faces", "GET /api/activities":
		write([]any{})
	case "GET /api/albums":
		if r.URL.Query().Get("assetId") == "original" || f.copied {
			write([]any{map[string]any{"id": "album", "albumThumbnailAssetId": "other"}})
		} else {
			write([]any{})
		}
	case "GET /api/shared-links":
		preview := f.sharedPreviewID
		if preview == "" {
			preview = "original"
		}
		// Immich's list contains only a preview, even after copying the link.
		write([]any{map[string]any{"id": "shared", "type": "INDIVIDUAL", "assets": []any{map[string]any{"id": preview}}}})
	case "GET /api/shared-links/shared":
		assets := []any{map[string]any{"id": "original"}}
		if f.copied && !f.omitSharedOutput {
			assets = append(assets, map[string]any{"id": "output"})
		}
		write(map[string]any{"id": "shared", "type": "INDIVIDUAL", "assets": assets})
	case "GET /api/assets/original/metadata":
		write([]any{map[string]any{"key": "user-note", "value": map[string]any{"nested": map[string]any{"label": "hello"}}}})
	case "GET /api/assets/output/metadata":
		items := []any{map[string]any{"key": "storage-saver-source", "value": f.provenance()}}
		for k, v := range f.targetMetadata {
			items = append(items, map[string]any{"key": k, "value": v})
		}
		write(items)
	case "GET /api/assets/output/metadata/storage-saver-source":
		write(map[string]any{"key": "storage-saver-source", "value": f.provenance()})
	case "PUT /api/assets/output/metadata":
		var body struct {
			Items []struct {
				Key   string         `json:"key"`
				Value map[string]any `json:"value"`
			} `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Error(err)
		}
		for _, item := range body.Items {
			f.targetMetadata[item.Key] = item.Value
		}
		write(body.Items)
	case "POST /api/assets/bulk-upload-check":
		if f.uploaded {
			write(map[string]any{"results": []any{map[string]any{"id": "output", "action": "reject", "reason": "duplicate", "assetId": "output"}}})
		} else {
			write(map[string]any{"results": []any{map[string]any{"id": "output", "action": "accept"}}})
		}
	case "POST /api/assets":
		f.assertStage(jobqueue.StagePending)
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			f.t.Error(err)
			http.Error(w, "invalid multipart", 400)
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		file, _, err := r.FormFile("assetData")
		if err != nil {
			f.t.Error(err)
			http.Error(w, "missing asset", 400)
			return
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || !bytes.Equal(data, f.output) {
			f.t.Errorf("uploaded bytes %q: %v", data, err)
		}
		var metadata []struct {
			Key   string         `json:"key"`
			Value map[string]any `json:"value"`
		}
		if err = json.Unmarshal([]byte(r.FormValue("metadata")), &metadata); err != nil || len(metadata) != 1 || metadata[0].Key != "storage-saver-source" || metadata[0].Value["sourceSHA1"] != f.job.SourceSHA1 {
			f.t.Errorf("upload provenance %v: %v", metadata, err)
		}
		f.uploaded = true
		f.uploads++
		write(map[string]any{"id": "output", "status": "created"})
	case "PUT /api/assets/copy":
		j, err := f.queue.Get(f.job.ID)
		if err != nil || (j.PublicationStage != jobqueue.StageVerified && j.PublicationStage != jobqueue.StageMetadataMigrated) {
			f.t.Errorf("copy before verification: %v %v", j.PublicationStage, err)
		}
		var body map[string]any
		if err = json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Error(err)
		}
		if body["sourceId"] != "original" || body["targetId"] != "output" || body["stack"] != false || body["sidecar"] != false {
			f.t.Errorf("unexpected copy: %v", body)
		}
		f.copied = true
		w.WriteHeader(http.StatusNoContent)
	case "PUT /api/assets":
		f.updated = true
		w.WriteHeader(http.StatusNoContent)
	case "PUT /api/assets/output":
		f.assertStage(jobqueue.StageMetadataMigrated)
		job, err := f.queue.Get(f.job.ID)
		if err != nil || job.SyncRepublished {
			f.t.Errorf("republish called after sync checkpoint: %+v: %v", job, err)
		}
		ledger, err := state.Open(f.ledgerDir)
		if err != nil {
			f.t.Error(err)
		} else if record, ok := ledger.FindSource(f.job.OwnerID, f.job.SourceSHA1); !ok || record.AssetID != "output" || record.State != "verified" {
			f.t.Errorf("republish before durable alias: %+v, found %v", record, ok)
		}
		var body map[string]any
		if err = json.NewDecoder(r.Body).Decode(&body); err != nil || !reflect.DeepEqual(body, map[string]any{"isFavorite": true}) {
			f.t.Errorf("republish changed output favorite: %v: %v", body, err)
		}
		f.republishAttempts++
		if f.failRepublish {
			http.Error(w, "republish unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "GET /api/server/features":
		write(map[string]any{"trash": true})
	case "DELETE /api/assets":
		f.assertStage(jobqueue.StageMetadataMigrated)
		if job, err := f.queue.Get(f.job.ID); err != nil || !job.SyncRepublished {
			f.t.Errorf("trash before sync checkpoint: %+v: %v", job, err)
		}
		// Reopen the ledger so this assertion proves persistence before trash.
		ledger, err := state.Open(f.ledgerDir)
		if err != nil {
			f.t.Error(err)
		} else if record, ok := ledger.FindSource(f.job.OwnerID, f.job.SourceSHA1); !ok || record.AssetID != "output" || record.State != "verified" {
			f.t.Errorf("trash before durable mapping: %+v, found %v", record, ok)
		}
		var body struct {
			IDs   []string `json:"ids"`
			Force bool     `json:"force"`
		}
		if err = json.NewDecoder(r.Body).Decode(&body); err != nil || !reflect.DeepEqual(body.IDs, []string{"original"}) || body.Force {
			f.t.Errorf("trash body %+v: %v", body, err)
		}
		f.trashed = true
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected publisher request %s %s", r.Method, r.URL)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

func (f *publisherFixture) checkpoint(stage jobqueue.PublicationStage) {
	f.t.Helper()
	for _, next := range []jobqueue.PublicationStage{jobqueue.StageUploaded, jobqueue.StageVerified, jobqueue.StageMetadataMigrated, jobqueue.StageOriginalTrashed} {
		if f.job.PublicationStage == stage {
			break
		}
		var err error
		f.job, err = f.queue.AdvancePublication(f.job.ID, f.job.PublicationStage, next, "output")
		if err != nil {
			f.t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploaded = stage != jobqueue.StagePending
	if stage == jobqueue.StageMetadataMigrated || stage == jobqueue.StageOriginalTrashed {
		f.copied = true
		f.updated = true
		f.targetMetadata["user-note"] = map[string]any{"nested": map[string]any{"label": "hello"}}
	}
	if stage == jobqueue.StageOriginalTrashed {
		f.trashed = true
	}
}

func (f *publisherFixture) restart() {
	f.t.Helper()
	if err := f.queue.Close(); err != nil {
		f.t.Fatal(err)
	}
	var err error
	f.queue, err = jobqueue.Open(f.queueDir, jobqueue.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	f.ledger, err = state.Open(f.ledgerDir)
	if err != nil {
		f.t.Fatal(err)
	}
	f.publisher.Queue = f.queue
	f.publisher.Ledger = f.ledger
	f.job, err = f.queue.Get(f.job.ID)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *publisherFixture) assertCompleted() {
	f.t.Helper()
	j, err := f.queue.Get(f.job.ID)
	if err != nil || j.PublicationStage != jobqueue.StageComplete || j.Status != jobqueue.StatusComplete || j.ResultAssetID != "output" || !j.SyncRepublished {
		f.t.Fatalf("incomplete publication: %+v: %v", j, err)
	}
	ledger, err := state.Open(f.ledgerDir)
	if err != nil {
		f.t.Fatal(err)
	}
	record, ok := ledger.FindSource(j.OwnerID, j.SourceSHA1)
	if !ok || record.AssetID != "output" || record.OutputSHA1 != j.OutputSHA1 || record.SourceSize != j.Size || record.OutputSize != j.OutputSize || record.State != "verified" {
		f.t.Fatalf("wrong durable mapping: %+v, found %v", record, ok)
	}
}

func TestPublisherHappyPathPersistsMappingBeforeTrash(t *testing.T) {
	f := newPublisherFixture(t)
	legacyHash, _ := publisherDigests([]byte("legacy import"))
	legacy := state.Record{OwnerID: "legacy-owner", AssetID: "legacy-asset", SourceSHA1: legacyHash, OutputSHA1: legacyHash, State: "verified", SourceFilename: "legacy.jpg"}
	if err := f.ledger.Put(legacy); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.Process(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.copied || !f.updated || !f.trashed || f.uploads != 1 || f.validations < 2 {
		t.Fatalf("publication missed work: copy=%v update=%v trash=%v uploads=%d validations=%d", f.copied, f.updated, f.trashed, f.uploads, f.validations)
	}
	if len(f.requests) == 0 || f.requests[len(f.requests)-1] != "DELETE /api/assets" {
		t.Fatal("trash was not the last HTTP action", f.requests)
	}
	ledger, err := state.Open(f.ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ledger.FindSource(legacy.OwnerID, legacy.SourceSHA1); !ok || !reflect.DeepEqual(got, legacy) {
		t.Fatal("legacy import mapping changed", got, ok)
	}
}

func TestPublisherChecksFullSharedLinkWhenPreviewHidesOriginal(t *testing.T) {
	for _, missingOutput := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_output_%v", missingOutput), func(t *testing.T) {
			f := newPublisherFixture(t)
			f.mu.Lock()
			f.sharedPreviewID = "unrelated"
			f.omitSharedOutput = missingOutput
			f.mu.Unlock()
			err := f.publisher.Process(context.Background(), f.job)
			if missingOutput {
				if err == nil || errors.Is(err, replacement.ErrBlocked) || !strings.Contains(err.Error(), "shared link") {
					t.Fatal("unrelated preview hid an unmigrated link", err)
				}
				j, getErr := f.queue.Get(f.job.ID)
				if getErr != nil || j.PublicationStage != jobqueue.StageVerified || j.SyncRepublished {
					t.Fatal("unmigrated link advanced publication", j, getErr)
				}
				if len(f.ledger.Records()) != 0 {
					t.Fatal("unmigrated link committed an alias")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				f.assertCompleted()
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			readDetail := false
			for _, request := range f.requests {
				if request == "GET /api/shared-links/shared" {
					readDetail = true
				}
			}
			if !readDetail {
				t.Fatal("publisher trusted the shared-link preview", f.requests)
			}
			if missingOutput && (f.trashed || f.republishAttempts != 0) {
				t.Fatal("unmigrated link reached retirement", f.requests)
			}
		})
	}
}

func TestPublisherResumesDurableCheckpointsWithoutAnotherUpload(t *testing.T) {
	for _, stage := range []jobqueue.PublicationStage{jobqueue.StageUploaded, jobqueue.StageVerified, jobqueue.StageMetadataMigrated} {
		t.Run(string(stage), func(t *testing.T) {
			f := newPublisherFixture(t)
			f.checkpoint(stage)
			f.restart()
			if err := f.publisher.Process(context.Background(), f.job); err != nil {
				t.Fatal(err)
			}
			f.assertCompleted()
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.uploads != 0 || !f.trashed {
				t.Fatalf("resume upload count=%d trashed=%v", f.uploads, f.trashed)
			}
			for _, request := range f.requests {
				if request == "POST /api/assets" || request == "POST /api/assets/bulk-upload-check" {
					t.Fatal("resume tried to create another asset", f.requests)
				}
			}
		})
	}
}

func TestPublisherPendingMetadataRetriesFromUploadedCheckpoint(t *testing.T) {
	f := newPublisherFixture(t)
	f.mu.Lock()
	f.outputPending = true
	f.mu.Unlock()
	err := f.publisher.Process(context.Background(), f.job)
	if err == nil || errors.Is(err, replacement.ErrBlocked) || !strings.Contains(err.Error(), "metadata extraction not ready") {
		t.Fatal("pending metadata should be retryable", err)
	}
	j, getErr := f.queue.Get(f.job.ID)
	if getErr != nil || j.Status != jobqueue.StatusPublishing || j.PublicationStage != jobqueue.StageUploaded || j.ResultAssetID != "output" {
		t.Fatal("pending metadata lost upload checkpoint", j, getErr)
	}
	f.mu.Lock()
	wrongProgress := f.copied || f.updated || f.trashed || f.uploads != 1
	f.mu.Unlock()
	if wrongProgress || len(f.ledger.Records()) != 0 {
		t.Fatal("publication progressed before extraction completed")
	}
	if _, err = f.queue.RetryPublication(j.ID, err.Error()); err != nil {
		t.Fatal(err)
	}
	if _, err = f.queue.NextPublication(); !errors.Is(err, jobqueue.ErrNoJobs) {
		t.Fatal("metadata retry did not back off", err)
	}
	f.restart()
	f.mu.Lock()
	f.outputPending = false
	f.mu.Unlock()
	// Process the persisted checkpoint directly; queue scheduling has its own tests.
	if err = f.publisher.Process(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads != 1 || !f.trashed {
		t.Fatal("metadata retry uploaded another replacement", f.uploads, f.trashed)
	}
}

func TestPublisherFailedRepublishRetainsOriginalAndRetriesSameOutput(t *testing.T) {
	f := newPublisherFixture(t)
	f.mu.Lock()
	f.failRepublish = true
	f.mu.Unlock()
	err := f.publisher.Process(context.Background(), f.job)
	if err == nil || errors.Is(err, replacement.ErrBlocked) || !strings.Contains(err.Error(), "503") {
		t.Fatal("failed republish was not retryable", err)
	}
	j, getErr := f.queue.Get(f.job.ID)
	if getErr != nil || j.Status != jobqueue.StatusPublishing || j.PublicationStage != jobqueue.StageMetadataMigrated || j.ResultAssetID != "output" || j.SyncRepublished {
		t.Fatal("failed republish lost metadata checkpoint", j, getErr)
	}
	ledger, openErr := state.Open(f.ledgerDir)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if record, ok := ledger.FindSource(j.OwnerID, j.SourceSHA1); !ok || record.AssetID != "output" || record.State != "verified" {
		t.Fatal("failed republish lost committed alias", record, ok)
	}
	for _, path := range []string{j.SourceSpoolPath, j.ResultPath} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatal("failed republish removed spool bytes", statErr)
		}
	}
	f.mu.Lock()
	prematureTrash := f.trashed
	attempts, uploads := f.republishAttempts, f.uploads
	f.mu.Unlock()
	if prematureTrash || attempts != 1 || uploads != 1 {
		t.Fatal("unexpected progress after failed republish", prematureTrash, attempts, uploads)
	}
	if _, err = f.queue.RetryPublication(j.ID, err.Error()); err != nil {
		t.Fatal(err)
	}
	f.restart()
	if f.job.SyncRepublished || f.job.PublicationStage != jobqueue.StageMetadataMigrated || f.job.ResultAssetID != "output" {
		t.Fatal("restart lost failed republish checkpoint", f.job)
	}
	f.mu.Lock()
	f.failRepublish = false
	f.mu.Unlock()
	if err = f.publisher.Process(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted()
	f.mu.Lock()
	attempts, uploads = f.republishAttempts, f.uploads
	trashed := f.trashed
	f.mu.Unlock()
	if attempts != 2 || uploads != 1 || !trashed {
		t.Fatal("retry changed output or skipped republish", attempts, uploads, trashed)
	}
	f.restart()
	if !f.job.SyncRepublished {
		t.Fatal("successful republish checkpoint lost after restart")
	}
}

func TestPublisherPersistedSyncCheckpointSkipsRepublish(t *testing.T) {
	f := newPublisherFixture(t)
	f.checkpoint(jobqueue.StageMetadataMigrated)
	if err := f.ledger.Put(state.Record{OwnerID: f.job.OwnerID, AssetID: "output", SourceSHA1: f.job.SourceSHA1, OutputSHA1: f.job.OutputSHA1, State: "verified", SourceSize: f.job.Size, OutputSize: f.job.OutputSize}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.queue.MarkSyncRepublished(f.job.ID); err != nil {
		t.Fatal(err)
	}
	f.restart()
	if !f.job.SyncRepublished {
		t.Fatal("missing persisted sync checkpoint")
	}
	if err := f.publisher.Process(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	f.assertCompleted()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.republishAttempts != 0 || f.uploads != 0 || !f.trashed {
		t.Fatal("checkpoint retry republished or uploaded", f.republishAttempts, f.uploads, f.trashed)
	}
	for _, request := range f.requests {
		if request == "PUT /api/assets/output" {
			t.Fatal("durable sync checkpoint issued another PUT", f.requests)
		}
	}
}

func TestPublisherOriginalTrashedCompletesWithoutNetwork(t *testing.T) {
	f := newPublisherFixture(t)
	f.checkpoint(jobqueue.StageOriginalTrashed)
	f.restart()
	f.publisher.RetireOriginal = false
	f.publisher.Client = nil
	f.publisher.Ledger = nil
	if err := f.publisher.Process(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	j, err := f.queue.Get(f.job.ID)
	if err != nil || j.Status != jobqueue.StatusComplete || j.PublicationStage != jobqueue.StageComplete {
		t.Fatal(j, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 || f.validations != 0 {
		t.Fatal("trashed checkpoint made external requests", f.requests, f.validations)
	}
}

func TestPublisherConflictingAliasBlocksRetirement(t *testing.T) {
	f := newPublisherFixture(t)
	f.checkpoint(jobqueue.StageMetadataMigrated)
	conflict := state.Record{OwnerID: f.job.OwnerID, AssetID: "unrelated-asset", SourceSHA1: f.job.SourceSHA1, OutputSHA1: f.job.OutputSHA1, State: "verified"}
	if err := f.ledger.Put(conflict); err != nil {
		t.Fatal(err)
	}
	f.restart()
	err := f.publisher.Process(context.Background(), f.job)
	if !errors.Is(err, replacement.ErrBlocked) || !strings.Contains(err.Error(), "conflicting source alias") {
		t.Fatal("conflicting alias not blocked", err)
	}
	j, getErr := f.queue.Get(f.job.ID)
	if getErr != nil || j.PublicationStage != jobqueue.StageMetadataMigrated {
		t.Fatal(j, getErr)
	}
	ledger, err := state.Open(f.ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ledger.FindSource(conflict.OwnerID, conflict.SourceSHA1); !ok || !reflect.DeepEqual(got, conflict) {
		t.Fatal("conflicting mapping overwritten", got, ok)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 || f.trashed {
		t.Fatal("conflicting alias reached retirement", f.requests)
	}
}

func TestPublisherRetirementDisabledBlocksBeforeReplacementUpload(t *testing.T) {
	f := newPublisherFixture(t)
	f.publisher.RetireOriginal = false
	err := f.publisher.Process(context.Background(), f.job)
	if !errors.Is(err, replacement.ErrBlocked) {
		t.Fatal("retirement disabled was not blocked", err)
	}
	// Stop after Run has logged the held job, avoiding its production ticker.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	logged := false
	f.publisher.Logger = log.New(publisherLogWriter(func(data []byte) (int, error) { logged = true; cancel(); return len(data), nil }), "", 0)
	f.publisher.Run(ctx)
	if !logged {
		t.Fatal("Run did not report the blocked publication")
	}
	f.restart()
	j, err := f.queue.Get(f.job.ID)
	if err != nil || j.Status != jobqueue.StatusHeld || j.PublicationStage != jobqueue.StagePending || j.ResultAssetID != "" {
		t.Fatal(j, err)
	}
	if len(f.ledger.Records()) != 0 {
		t.Fatal("disabled replacement changed alias ledger")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 || f.uploads != 0 || f.validations != 0 {
		t.Fatal("disabled replacement reached network or codec", f.requests, f.uploads, f.validations)
	}
}

type publisherLogWriter func([]byte) (int, error)

func (write publisherLogWriter) Write(data []byte) (int, error) { return write(data) }

func TestPublisherReconciliationRotatesPastFailedIntents(t *testing.T) {
	queue, err := jobqueue.Open(t.TempDir(), jobqueue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer queue.Close()
	want := map[string]bool{}
	for _, source := range []string{"first pending original", "second pending original"} {
		h1, h256 := publisherDigests([]byte(source))
		j, _, err := queue.Enqueue(jobqueue.Input{OwnerID: "owner", SourceSHA1: h1, SourceSHA256: h256, Size: int64(len(source)), Filename: "source.jpg", Preset: "balanced"}, strings.NewReader(source))
		if err != nil {
			t.Fatal(err)
		}
		want[j.ID] = true
	}
	var tried []string
	p := Publisher{Queue: queue, ReconcileOriginal: func(_ context.Context, j jobqueue.Job) (string, error) {
		tried = append(tried, j.ID)
		return "", errors.New("upstream unavailable")
	}}
	p.reconcileOne(context.Background())
	p.reconcileOne(context.Background())
	if len(tried) != 2 || tried[0] == tried[1] || !want[tried[0]] || !want[tried[1]] {
		t.Fatal("failed first intent starved second", tried)
	}
	for _, j := range queue.List() {
		if j.Status != jobqueue.StatusPublishingOriginal || j.OriginalAssetID != "" {
			t.Fatal("failed reconciliation changed intent", j)
		}
	}
	p.ReconcileOriginal = func(_ context.Context, j jobqueue.Job) (string, error) { return "original-" + j.ID, nil }
	p.reconcileOne(context.Background())
	p.reconcileOne(context.Background())
	for _, j := range queue.List() {
		if j.Status != jobqueue.StatusQueued || j.OriginalAssetID != "original-"+j.ID {
			t.Fatal("reconciliation did not publish original ID", j)
		}
	}
}

func TestPublisherCollectOneReleasesVerifiedCompleteJob(t *testing.T) {
	f := newPublisherFixture(t)
	if err := f.publisher.Process(context.Background(), f.job); err != nil {
		t.Fatal(err)
	}
	before, err := f.queue.Get(f.job.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.requests = nil
	validations := f.validations
	f.mu.Unlock()
	if err = f.publisher.CollectOne(context.Background()); err != nil {
		t.Fatal(err)
	}
	released, err := f.queue.Get(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.SourceSpoolPath = ""
	want.ResultPath = ""
	want.UpdatedAt = released.UpdatedAt
	if !reflect.DeepEqual(released, want) {
		t.Fatal("cleanup changed durable job metadata", released, want)
	}
	for _, path := range []string{before.SourceSpoolPath, before.ResultPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("verified complete job retained blob", path, err)
		}
	}
	f.mu.Lock()
	requests := append([]string(nil), f.requests...)
	codecCalls := f.validations
	f.mu.Unlock()
	if !reflect.DeepEqual(requests, []string{"GET /api/assets/output", "GET /api/assets/output/metadata/storage-saver-source", "GET /api/assets/output/original"}) || codecCalls != validations {
		t.Fatal("cleanup did not verify the remote output independently", requests, codecCalls)
	}
	f.restart()
	f.assertCompleted()
	if _, err = f.queue.Complete(before.ID, before.Lease.WorkerID, before.Lease.Token, jobqueue.ResultInput{SHA1: before.OutputSHA1, SHA256: before.OutputSHA256, Size: before.OutputSize, Filename: before.OutputFilename}, nil); err != nil {
		t.Fatal("released completed result stopped being idempotent", err)
	}
	if got, created, err := f.queue.Enqueue(before.Input, nil); err != nil || created || got.ID != before.ID || got.SourceSpoolPath != "" {
		t.Fatal("cleanup lost the source tombstone", got, created, err)
	}
	if err = f.publisher.CollectOne(context.Background()); err != nil {
		t.Fatal("repeat cleanup", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != len(requests) {
		t.Fatal("released job was verified again", f.requests)
	}
}

func TestPublisherCollectOneKeepsSkippedSourceOnRemoteMismatch(t *testing.T) {
	for _, mismatch := range []string{"owner", "hash"} {
		t.Run(mismatch, func(t *testing.T) {
			dir := t.TempDir()
			queue, err := jobqueue.Open(dir, jobqueue.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = queue.Close() }()
			source := []byte("skipped original")
			h1, h256 := publisherDigests(source)
			j, _, err := queue.Enqueue(jobqueue.Input{OriginalAssetID: "original", OwnerID: "owner", SourceSHA1: h1, SourceSHA256: h256, Size: int64(len(source)), Filename: "photo.jpg", Preset: "balanced"}, bytes.NewReader(source))
			if err != nil {
				t.Fatal(err)
			}
			j, err = queue.Claim("worker")
			if err != nil {
				t.Fatal(err)
			}
			j, err = queue.MarkSkipped(j.ID, "worker", j.Lease.Token, "no conversion gain")
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/assets/original" {
					t.Errorf("unexpected cleanup request %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", 500)
					return
				}
				owner := j.OwnerID
				hash := j.SourceSHA1
				if mismatch == "owner" {
					owner = "different-owner"
				} else {
					hash, _ = publisherDigests([]byte("different original"))
				}
				raw, _ := hex.DecodeString(hash)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": j.OriginalAssetID, "ownerId": owner, "checksum": base64.StdEncoding.EncodeToString(raw)})
			}))
			defer server.Close()
			p := Publisher{Queue: queue, Client: replacement.NewClient(server.URL, "key", server.Client())}
			if err = p.CollectOne(context.Background()); !errors.Is(err, replacement.ErrBlocked) {
				t.Fatal("remote mismatch did not block release", err)
			}
			got, err := queue.Get(j.ID)
			if err != nil || !reflect.DeepEqual(got, j) {
				t.Fatal("blocked cleanup changed intent", got, err)
			}
			data, err := os.ReadFile(j.SourceSpoolPath)
			if err != nil || !bytes.Equal(data, source) {
				t.Fatal("blocked cleanup removed source", string(data), err)
			}
			if err = queue.Close(); err != nil {
				t.Fatal(err)
			}
			queue, err = jobqueue.Open(dir, jobqueue.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if got, err = queue.Get(j.ID); err != nil || got.SourceSpoolPath != j.SourceSpoolPath || got.Status != jobqueue.StatusSkipped {
				t.Fatal("blocked cleanup lost source on restart", got, err)
			}
		})
	}
}
