package jobqueue

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }
func newClock() *testClock                   { return &testClock{at: time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC)} }

func inputFor(body string) Input {
	h1, h256 := sha1.Sum([]byte(body)), sha256.Sum256([]byte(body))
	return Input{OriginalAssetID: "original-id", OwnerID: "owner", SourceSHA1: hex.EncodeToString(h1[:]), SourceSHA256: hex.EncodeToString(h256[:]), Size: int64(len(body)), Filename: "../photo.jpg", Preset: "balanced", SourcePath: "../../outside.jpg", UploadFields: map[string][]string{"isFavorite": {"true"}, "albumIds": {"a", "b"}}}
}
func resultFor(body string) ResultInput {
	in := inputFor(body)
	return ResultInput{SHA1: in.SourceSHA1, SHA256: in.SourceSHA256, Size: in.Size, Filename: "result.avif"}
}
func openTest(t *testing.T, dir string, options Options) *Store {
	t.Helper()
	s, err := Open(dir, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func enqueueTest(t *testing.T, s *Store, body string) Job {
	t.Helper()
	j, created, err := s.Enqueue(inputFor(body), strings.NewReader(body))
	if err != nil || !created {
		t.Fatalf("enqueue created=%v: %v", created, err)
	}
	return j
}
func claimTest(t *testing.T, s *Store, worker string) Job {
	t.Helper()
	j, err := s.Claim(worker)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func completeTest(t *testing.T, s *Store, j Job) Job {
	t.Helper()
	j, err := s.Complete(j.ID, j.Lease.WorkerID, j.Lease.Token, resultFor("result"), strings.NewReader("result"))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestIngestionIntentPersistenceAndNoPrematureClaim(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	in := inputFor("original")
	in.OriginalAssetID = ""
	j, created, err := s.Enqueue(in, strings.NewReader("original"))
	if err != nil || !created {
		t.Fatalf("%v %v", created, err)
	}
	if j.Status != StatusPublishingOriginal {
		t.Fatal(j.Status)
	}
	if _, err = s.Claim("worker"); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, Options{})
	got, err := s.Get(j.ID)
	if err != nil || !reflect.DeepEqual(j, got) {
		t.Fatalf("persisted intent mismatch: %v", err)
	}
	if _, err = s.Claim("worker"); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
	got, err = s.MarkOriginalUploaded(j.ID, "real-immich-id")
	if err != nil || got.Status != StatusQueued {
		t.Fatal(got.Status, err)
	}
	if _, err = s.MarkOriginalUploaded(j.ID, "different-id"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.MarkOriginalUploaded(j.ID, "real-immich-id"); err != nil {
		t.Fatal(err)
	}
	claimed := claimTest(t, s, "worker")
	if claimed.OriginalAssetID != "real-immich-id" {
		t.Fatal(claimed.OriginalAssetID)
	}
	r, err := s.OpenSource(j.ID, "worker", claimed.Lease.Token)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(data) != "original" {
		t.Fatal(string(data), err)
	}
	if filepath.Dir(j.SourceSpoolPath) != filepath.Join(dir, "spool") {
		t.Fatal("caller path influenced spool")
	}
}

func TestDuplicateAndReturnedMetadataAreIsolated(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{})
	j := enqueueTest(t, s, "source")
	in := inputFor("source")
	in.SourceSHA1 = strings.ToUpper(in.SourceSHA1)
	in.SourceSHA256 = strings.ToUpper(in.SourceSHA256)
	got, created, err := s.Enqueue(in, nil)
	if err != nil || created || got.ID != j.ID {
		t.Fatalf("duplicate: %v %v", created, err)
	}
	got.UploadFields["albumIds"][0] = "changed"
	got, err = s.Get(j.ID)
	if err != nil || got.UploadFields["albumIds"][0] != "a" {
		t.Fatal(got, err)
	}
	list := s.List()
	list[0].UploadFields["albumIds"][0] = "changed"
	got, _ = s.Get(j.ID)
	if got.UploadFields["albumIds"][0] != "a" {
		t.Fatal("List leaked map")
	}
	in.OwnerID = "other-owner"
	if _, created, err = s.Enqueue(in, strings.NewReader("source")); err != nil || !created {
		t.Fatal(created, err)
	}
}

func TestRestartPreservesLeaseAndPublicationCheckpoints(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	opts := Options{Now: clock.now, LeaseDuration: time.Minute, RetryBase: time.Second, RetryMax: time.Minute}
	s := openTest(t, dir, opts)
	enqueueTest(t, s, "source")
	j := claimTest(t, s, "worker")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, opts)
	if _, err := s.Claim("another"); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
	if _, err := s.Heartbeat(j.ID, "worker", j.Lease.Token); err != nil {
		t.Fatal(err)
	}
	j = completeTest(t, s, j)
	stages := []PublicationStage{StageUploaded, StageVerified, StageMetadataMigrated, StageOriginalTrashed, StageComplete}
	for _, next := range stages {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s = openTest(t, dir, opts)
		got, err := s.NextPublication()
		if err != nil || got.PublicationStage != j.PublicationStage {
			t.Fatalf("resume %v: %v", next, err)
		}
		old := j.PublicationStage
		j, err = s.AdvancePublication(j.ID, old, next, "new-asset")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.AdvancePublication(j.ID, old, next, "new-asset"); err != nil {
			t.Fatalf("checkpoint idempotence: %v", err)
		}
	}
	if j.Status != StatusComplete {
		t.Fatal(j.Status)
	}
	if _, err := s.NextPublication(); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
	if _, err := s.AdvancePublication(j.ID, StagePending, StageOriginalTrashed, ""); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, opts)
	clock.advance(24 * time.Hour)
	if _, err := s.Complete(j.ID, "worker", j.Lease.Token, resultFor("result"), nil); err != nil {
		t.Fatalf("accepted completion retry: %v", err)
	}
	if _, err := s.Complete(j.ID, "wrong-worker", j.Lease.Token, resultFor("result"), nil); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	if _, err := s.Complete(j.ID, "worker", "wrong-token", resultFor("result"), nil); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	if _, err := s.Complete(j.ID, "worker", j.Lease.Token, resultFor("different"), nil); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
}

func TestLeaseExpiryReclaimAndStaleGeneration(t *testing.T) {
	clock := newClock()
	s := openTest(t, t.TempDir(), Options{Now: clock.now, LeaseDuration: time.Minute, RetryBase: time.Second, RetryMax: 4 * time.Second, MaxAttempts: 3})
	enqueueTest(t, s, "source")
	old := claimTest(t, s, "worker")
	clock.advance(time.Minute)
	if _, err := s.Heartbeat(old.ID, "worker", old.Lease.Token); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	if _, err := s.OpenSource(old.ID, "worker", old.Lease.Token); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	if _, err := s.Complete(old.ID, "worker", old.Lease.Token, resultFor("result"), strings.NewReader("result")); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	if _, err := s.Claim("worker"); !errors.Is(err, ErrNoJobs) {
		t.Fatal("expiry must back off", err)
	}
	clock.advance(time.Second)
	j := claimTest(t, s, "worker")
	if j.Lease.Token == old.Lease.Token || j.Attempts != 2 {
		t.Fatal("lease generation did not change")
	}
	if _, err := s.Fail(old.ID, "worker", old.Lease.Token, "late failure"); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	j = completeTest(t, s, j)
	if _, err := s.Complete(old.ID, "worker", old.Lease.Token, resultFor("result"), nil); !errors.Is(err, ErrLease) {
		t.Fatal(err)
	}
	clock.advance(time.Hour)
	if _, err := s.Complete(j.ID, "worker", j.Lease.Token, resultFor("result"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestFailureBackoffExhaustionAndSkip(t *testing.T) {
	clock := newClock()
	s := openTest(t, t.TempDir(), Options{Now: clock.now, RetryBase: time.Second, RetryMax: 2 * time.Second, MaxAttempts: 3})
	enqueueTest(t, s, "source")
	for attempt := 1; attempt <= 3; attempt++ {
		j := claimTest(t, s, "worker")
		if j.Attempts != attempt {
			t.Fatal(j.Attempts)
		}
		failed, err := s.Fail(j.ID, "worker", j.Lease.Token, strings.Repeat("e", 5000))
		if err != nil {
			t.Fatal(err)
		}
		if len(failed.LastError) != 4096 {
			t.Fatal("unbounded error")
		}
		if _, err = s.Claim("worker"); !errors.Is(err, ErrNoJobs) {
			t.Fatal(err)
		}
		if attempt == 3 {
			if failed.Status != StatusFailed {
				t.Fatal(failed.Status)
			}
		} else {
			if failed.Status != StatusQueued {
				t.Fatal(failed.Status)
			}
			clock.advance(time.Duration(attempt) * time.Second)
		}
	}
	enqueueTest(t, s, "skip-source")
	j := claimTest(t, s, "worker")
	skipped, err := s.MarkSkipped(j.ID, "worker", j.Lease.Token, "no conversion gain")
	if err != nil || skipped.Status != StatusSkipped {
		t.Fatal(skipped.Status, err)
	}
	clock.advance(24 * time.Hour)
	if _, err = s.Claim("worker"); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
	if _, err = s.NextPublication(); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
}

func TestPublicationBackoffAndDurableHold(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	options := Options{Now: clock.now, RetryBase: time.Second, MaxAttempts: 2}
	s := openTest(t, dir, options)
	enqueueTest(t, s, "source")
	j := completeTest(t, s, claimTest(t, s, "worker"))
	if _, err := s.AdvancePublication(j.ID, StagePending, StageUploaded, j.OriginalAssetID); err == nil {
		t.Fatal("accepted original as replacement")
	}
	if _, err := s.AdvancePublication(j.ID, StagePending, StageVerified, "new"); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	j, err := s.AdvancePublication(j.ID, StagePending, StageUploaded, "replacement")
	if err != nil {
		t.Fatal(err)
	}
	j, err = s.RetryPublication(j.ID, "transient")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPublication(); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if _, err = s.NextPublication(); err != nil {
		t.Fatal(err)
	}
	j, err = s.RetryPublication(j.ID, "again")
	if err != nil || j.Status != StatusHeld {
		t.Fatal(j.Status, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, options)
	clock.advance(24 * time.Hour)
	if _, err = s.NextPublication(); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
	j, err = s.ResumePublication(j.ID)
	if err != nil || j.PublicationStage != StageUploaded || j.ResultAssetID != "replacement" {
		t.Fatal(j, err)
	}
	if _, err = s.HoldPublication(j.ID, "blocked relation migration"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NextPublication(); !errors.Is(err, ErrNoJobs) {
		t.Fatal(err)
	}
}

func TestAdmissionBoundsAndHashValidation(t *testing.T) {
	t.Run("jobs", func(t *testing.T) {
		s := openTest(t, t.TempDir(), Options{MaxJobs: 1})
		enqueueTest(t, s, "first")
		if _, _, err := s.Enqueue(inputFor("second"), strings.NewReader("second")); !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
		if _, created, err := s.Enqueue(inputFor("first"), nil); err != nil || created {
			t.Fatal(created, err)
		}
	})
	t.Run("source", func(t *testing.T) {
		s := openTest(t, t.TempDir(), Options{MaxSourceBytes: 3})
		if _, _, err := s.Enqueue(inputFor("four"), strings.NewReader("four")); !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
	})
	t.Run("total", func(t *testing.T) {
		s := openTest(t, t.TempDir(), Options{MaxSpoolBytes: 8})
		enqueueTest(t, s, "source")
		j := claimTest(t, s, "worker")
		if _, err := s.Complete(j.ID, "worker", j.Lease.Token, resultFor("result"), strings.NewReader("result")); !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
	})
	t.Run("result", func(t *testing.T) {
		s := openTest(t, t.TempDir(), Options{MaxResultBytes: 3})
		enqueueTest(t, s, "source")
		j := claimTest(t, s, "worker")
		if _, err := s.Complete(j.ID, "worker", j.Lease.Token, resultFor("result"), strings.NewReader("result")); !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
	})
	for _, body := range []string{"short", "source plus excess", "broken"} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir, Options{})
			if _, _, err := s.Enqueue(inputFor("source"), strings.NewReader(body)); err == nil {
				t.Fatal("accepted invalid content")
			}
			if len(s.List()) != 0 {
				t.Fatal("invalid job persisted")
			}
			entries, err := os.ReadDir(filepath.Join(dir, "spool"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed write leaked spool: %v %v", entries, err)
			}
		})
	}
	t.Run("metadata", func(t *testing.T) {
		s := openTest(t, t.TempDir(), Options{})
		in := inputFor("source")
		in.UploadFields = map[string][]string{"oversize": {strings.Repeat("x", 65536)}}
		if _, _, err := s.Enqueue(in, strings.NewReader("source")); err == nil {
			t.Fatal("unbounded metadata")
		}
	})
}

func TestRecoveryDeletesOnlyOwnedOrphansAndRejectsCorruption(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	j := enqueueTest(t, s, "source")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "spool", "blob-orphan")
	unknown := filepath.Join(dir, "spool", "unrelated")
	temp := filepath.Join(dir, "queue-tmp-interrupted")
	for _, p := range []string{orphan, unknown, temp} {
		if err := os.WriteFile(p, []byte("garbage"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s = openTest(t, dir, Options{})
	for _, p := range []string{orphan, temp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("orphan not reclaimed", p, err)
		}
	}
	for _, p := range []string{unknown, j.SourceSpoolPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.SourceSpoolPath, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(dir, Options{}); err == nil {
		_ = opened.Close()
		t.Fatal("accepted truncated spool")
	}
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(dir, Options{}); err == nil {
		_ = opened.Close()
		t.Fatal("accepted corrupted snapshot")
	}
}

func TestPersistFailurePoisonsStoreWithoutMemoryRollback(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	enqueueTest(t, s, "source")
	state := filepath.Join(dir, "queue.json")
	backup := filepath.Join(dir, "saved.json")
	if err := os.Rename(state, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim("worker"); err == nil {
		t.Fatal("expected rename failure")
	}
	if _, err := s.Claim("worker"); !errors.Is(err, ErrClosed) {
		t.Fatal("uncertain store continued writes", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, state); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, Options{})
	j := claimTest(t, s, "worker")
	if j.Attempts != 1 {
		t.Fatal("failed save changed durable state")
	}
}

func TestSnapshotLimitRejectsNewBlobWithoutStoppingHeartbeats(t *testing.T) {
	for _, operation := range []string{"enqueue", "complete"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			clock := newClock()
			s := openTest(t, dir, Options{Now: clock.now})
			enqueueTest(t, s, "source")
			j := claimTest(t, s, "worker")
			before, err := os.ReadFile(filepath.Join(dir, "queue.json"))
			if err != nil {
				t.Fatal(err)
			}
			// Leave enough space for this job's heartbeat but not another source
			// or the result metadata. The public limit remains 128 MiB.
			s.snapshotLimit = int64(len(before) + 128)
			if operation == "enqueue" {
				_, _, err = s.Enqueue(inputFor("another source"), strings.NewReader("another source"))
			} else {
				_, err = s.Complete(j.ID, "worker", j.Lease.Token, resultFor("result"), strings.NewReader("result"))
			}
			if !errors.Is(err, ErrLimit) {
				t.Fatal("expected snapshot limit rejection", err)
			}
			if s.poisoned {
				t.Fatal("known pre-write rejection poisoned store")
			}
			after, err := os.ReadFile(filepath.Join(dir, "queue.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejection changed durable snapshot", err)
			}
			got, err := s.Get(j.ID)
			if err != nil || !reflect.DeepEqual(got, j) || len(s.List()) != 1 {
				t.Fatal("rejection changed memory", got, err)
			}
			entries, err := os.ReadDir(filepath.Join(dir, "spool"))
			if err != nil || len(entries) != 1 {
				t.Fatal("rejected blob was not removed", entries, err)
			}
			clock.advance(time.Second)
			got, err = s.Heartbeat(j.ID, "worker", j.Lease.Token)
			if err != nil || !got.Lease.ExpiresAt.After(j.Lease.ExpiresAt) {
				t.Fatal("existing lease could not heartbeat after rejection", got.Lease, err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTest(t, dir, Options{Now: clock.now})
			persisted, err := s.Get(j.ID)
			if err != nil || !persisted.Lease.ExpiresAt.Equal(got.Lease.ExpiresAt) {
				t.Fatal("heartbeat was not persisted", persisted.Lease, err)
			}
		})
	}
}

func TestPreRenameFailureLeavesStoreUsable(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	s := openTest(t, dir, Options{Now: clock.now})
	enqueueTest(t, s, "source")
	j := claimTest(t, s, "worker")
	clock.advance(time.Second)
	// A missing snapshot directory makes CreateTemp fail before rename.
	s.dir = filepath.Join(dir, "missing")
	if _, err := s.Heartbeat(j.ID, "worker", j.Lease.Token); err == nil {
		t.Fatal("expected pre-rename I/O failure")
	}
	s.dir = dir
	if s.poisoned {
		t.Fatal("pre-rename failure poisoned store")
	}
	got, err := s.Get(j.ID)
	if err != nil || !reflect.DeepEqual(got, j) {
		t.Fatal("failed snapshot changed job", got, err)
	}
	if _, err = s.Heartbeat(j.ID, "worker", j.Lease.Token); err != nil {
		t.Fatal("retry after pre-rename failure", err)
	}
}

func TestUncertainSnapshotFailureRetainsNewBlobForRecovery(t *testing.T) {
	for _, operation := range []string{"enqueue", "complete"} {
		for _, failure := range []string{"rename", "directory_sync"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				dir := t.TempDir()
				s := openTest(t, dir, Options{})
				enqueueTest(t, s, "source")
				leased := claimTest(t, s, "worker")
				injected := errors.New("snapshot commit outcome uncertain")
				if failure == "rename" {
					s.replaceSnapshot = func(from, to string) error {
						if err := atomicReplace(from, to); err != nil {
							return err
						}
						return injected // Simulate a failed acknowledgement after rename.
					}
				} else {
					s.syncSnapshotDir = func(string) error { return injected }
				}
				var err error
				if operation == "enqueue" {
					_, _, err = s.Enqueue(inputFor("another source"), strings.NewReader("another source"))
				} else {
					_, err = s.Complete(leased.ID, "worker", leased.Lease.Token, resultFor("result"), strings.NewReader("result"))
				}
				if !errors.Is(err, injected) || !s.poisoned {
					t.Fatal("uncertain commit was not held for recovery", err)
				}
				if _, err = s.Heartbeat(leased.ID, "worker", leased.Lease.Token); !errors.Is(err, ErrClosed) {
					t.Fatal("uncertain writer accepted another snapshot", err)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				s = openTest(t, dir, Options{})
				var recovered Job
				if operation == "enqueue" {
					var found bool
					recovered, found, err = s.FindIntent("owner", inputFor("another source").SourceSHA256)
					if err != nil || !found || len(s.List()) != 2 {
						t.Fatal("committed intent did not recover", found, err)
					}
				} else {
					recovered, err = s.Get(leased.ID)
					if err != nil || recovered.Status != StatusPublishing || recovered.OutputSHA256 != resultFor("result").SHA256 {
						t.Fatal("committed result did not recover", recovered, err)
					}
				}
				path := recovered.SourceSpoolPath
				want := "another source"
				if operation == "complete" {
					path = recovered.ResultPath
					want = "result"
				}
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatal("uncertain commit lost its referenced blob", string(data), err)
				}
				entries, err := os.ReadDir(filepath.Join(dir, "spool"))
				if err != nil || len(entries) != 2 {
					t.Fatal("recovered blob count", entries, err)
				}
			})
		}
	}
}

func TestExclusiveProcessLock(t *testing.T) {
	if dir := os.Getenv("JOBQUEUE_LOCK_HELPER"); dir != "" {
		s, err := Open(dir, Options{})
		if s != nil {
			_ = s.Close()
		}
		if !errors.Is(err, ErrLocked) {
			t.Fatalf("second process acquired queue: %v", err)
		}
		return
	}
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	if other, err := Open(dir, Options{}); !errors.Is(err, ErrLocked) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatal("second store acquired lock", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExclusiveProcessLock$")
	cmd.Env = append(os.Environ(), "JOBQUEUE_LOCK_HELPER="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cross-process lock: %s: %v", out, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_ = openTest(t, dir, Options{})
}

func TestAbruptProcessExitRecovery(t *testing.T) {
	if dir := os.Getenv("JOBQUEUE_CRASH_HELPER"); dir != "" {
		s, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = s.Enqueue(inputFor("source"), strings.NewReader("source")); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Claim("crashed-worker"); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Deliberately bypass Close and test cleanup.
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAbruptProcessExitRecovery$")
	cmd.Env = append(os.Environ(), "JOBQUEUE_CRASH_HELPER="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %s: %v", out, err)
	}
	s := openTest(t, dir, Options{})
	jobs := s.List()
	if len(jobs) != 1 || jobs[0].Status != StatusLeased || jobs[0].Lease.WorkerID != "crashed-worker" {
		t.Fatal("crash lost committed job/lease", jobs)
	}
	j, found, err := s.FindIntent("owner", strings.ToUpper(inputFor("source").SourceSHA256))
	if err != nil || !found || j.ID != jobs[0].ID {
		t.Fatal("intent lookup after crash", found, err)
	}
	if _, created, err := s.Enqueue(inputFor("source"), nil); err != nil || created {
		t.Fatal("duplicate after crash", created, err)
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	body    *strings.Reader
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started); <-r.release })
	return r.body.Read(p)
}

func TestHeartbeatDuringResultTransferAndStaleTransferRejected(t *testing.T) {
	clock := newClock()
	s := openTest(t, t.TempDir(), Options{Now: clock.now, LeaseDuration: time.Minute, RetryBase: time.Second})
	enqueueTest(t, s, "source")
	j := claimTest(t, s, "worker")
	r := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), body: strings.NewReader("result")}
	done := make(chan error, 1)
	go func() { _, err := s.Complete(j.ID, "worker", j.Lease.Token, resultFor("result"), r); done <- err }()
	<-r.started
	clock.advance(30 * time.Second)
	if _, err := s.Heartbeat(j.ID, "worker", j.Lease.Token); err != nil {
		close(r.release)
		t.Fatal(err)
	}
	clock.advance(time.Minute)
	if _, err := s.Claim("other"); !errors.Is(err, ErrNoJobs) {
		close(r.release)
		t.Fatal(err)
	}
	clock.advance(time.Second)
	newLease := claimTest(t, s, "worker")
	if newLease.Lease.Token == j.Lease.Token {
		t.Fatal("token reused")
	}
	close(r.release)
	if err := <-done; !errors.Is(err, ErrLease) {
		t.Fatal("late streamed result accepted", err)
	}
	if len(s.List()) != 1 {
		t.Fatal("unexpected jobs")
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "spool"))
	if err != nil || len(entries) != 1 {
		t.Fatal("rejected result leaked spool", entries, err)
	}
}

func TestConcurrentAdmissionReservations(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{MaxSpoolBytes: 8, MaxJobs: 2})
	r := &blockingReader{started: make(chan struct{}), release: make(chan struct{}), body: strings.NewReader("source")}
	done := make(chan error, 1)
	go func() { _, _, err := s.Enqueue(inputFor("source"), r); done <- err }()
	<-r.started
	if _, _, err := s.Enqueue(inputFor("other"), strings.NewReader("other")); !errors.Is(err, ErrLimit) {
		close(r.release)
		t.Fatal("inflight bytes not reserved", err)
	}
	close(r.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSameSourceHasOneJob(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := s.Enqueue(inputFor("source"), bytes.NewBufferString("source"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(s.List()) != 1 {
		t.Fatal("duplicate owner/source jobs")
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "spool"))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
}

func TestRecoveryRejectsOutsideSpoolPath(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{})
	j := enqueueTest(t, s, "source")
	_ = s.Close()
	j.SourceSpoolPath = filepath.Join(t.TempDir(), "blob-external")
	if err := os.WriteFile(j.SourceSpoolPath, []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(snapshot{Version: stateVersion, Jobs: map[string]Job{j.ID: j}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "queue.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if opened, err := Open(dir, Options{}); err == nil {
		_ = opened.Close()
		t.Fatal("accepted external spool reference")
	}
}
