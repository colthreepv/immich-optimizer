package jobqueue

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func completedForRelease(t *testing.T, s *Store) Job {
	t.Helper()
	enqueueTest(t, s, "source")
	j := completeTest(t, s, claimTest(t, s, "worker"))
	for _, next := range []PublicationStage{StageUploaded, StageVerified, StageMetadataMigrated, StageOriginalTrashed, StageComplete} {
		var err error
		j, err = s.AdvancePublication(j.ID, j.PublicationStage, next, "replacement")
		if err != nil {
			t.Fatal(err)
		}
	}
	return j
}

func TestReleaseSpoolCompletedTombstoneAndResultRetrySurviveReopen(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	s := openTest(t, dir, Options{Now: clock.now})
	before := completedForRelease(t, s)
	clock.advance(24 * time.Hour)
	released, err := s.ReleaseSpool(before.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.SourceSpoolPath = ""
	want.ResultPath = ""
	want.UpdatedAt = released.UpdatedAt
	if !reflect.DeepEqual(released, want) {
		t.Fatal("release changed immutable metadata", released, want)
	}
	for _, path := range []string{before.SourceSpoolPath, before.ResultPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("blob retained", path, err)
		}
	}
	if s.usedBytes() != 0 {
		t.Fatal("released bytes still accounted", s.usedBytes())
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, Options{Now: clock.now})
	got, err := s.Complete(before.ID, "worker", before.Lease.Token, resultFor("result"), nil)
	if err != nil || !reflect.DeepEqual(got, released) {
		t.Fatal("accepted result retry failed after release", got, err)
	}
	if _, err = s.Complete(before.ID, "other-worker", before.Lease.Token, resultFor("result"), nil); !errors.Is(err, ErrLease) {
		t.Fatal("wrong worker accepted", err)
	}
	if got, created, err := s.Enqueue(inputFor("source"), nil); err != nil || created || got.ID != before.ID || got.SourceSpoolPath != "" {
		t.Fatal("released tombstone did not deduplicate", got, created, err)
	}
	if _, err = s.ReleaseSpool(before.ID); err != nil {
		t.Fatal("repeat release", err)
	}
}

func TestReleaseSpoolSkippedAndFailed(t *testing.T) {
	for _, status := range []Status{StatusSkipped, StatusFailed} {
		t.Run(string(status), func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir, Options{MaxAttempts: 1})
			enqueueTest(t, s, "source")
			j := claimTest(t, s, "worker")
			var err error
			if status == StatusSkipped {
				j, err = s.MarkSkipped(j.ID, "worker", j.Lease.Token, "no gain")
			} else {
				j, err = s.Fail(j.ID, "worker", j.Lease.Token, "conversion failed")
			}
			if err != nil {
				t.Fatal(err)
			}
			released, err := s.ReleaseSpool(j.ID)
			if err != nil || released.Status != status || released.SourceSpoolPath != "" || released.SourceSHA256 != j.SourceSHA256 || released.OriginalAssetID != j.OriginalAssetID {
				t.Fatal(released, err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTest(t, dir, Options{})
			if got, err := s.Get(j.ID); err != nil || got.SourceSpoolPath != "" || got.Status != status {
				t.Fatal(got, err)
			}
		})
	}
}

func TestReleaseSpoolRejectsNonterminalJobs(t *testing.T) {
	for _, status := range []Status{StatusPublishingOriginal, StatusQueued, StatusLeased, StatusPublishing, StatusHeld} {
		t.Run(string(status), func(t *testing.T) {
			s := openTest(t, t.TempDir(), Options{})
			in := inputFor("source")
			if status == StatusPublishingOriginal {
				in.OriginalAssetID = ""
			}
			j, _, err := s.Enqueue(in, strings.NewReader("source"))
			if err != nil {
				t.Fatal(err)
			}
			if status == StatusLeased || status == StatusPublishing || status == StatusHeld {
				j = claimTest(t, s, "worker")
			}
			if status == StatusPublishing || status == StatusHeld {
				j = completeTest(t, s, j)
			}
			if status == StatusHeld {
				j, err = s.HoldPublication(j.ID, "needs review")
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = s.ReleaseSpool(j.ID); !errors.Is(err, ErrConflict) {
				t.Fatal("released nonterminal job", err)
			}
			got, err := s.Get(j.ID)
			if err != nil || !reflect.DeepEqual(j, got) {
				t.Fatal("rejection changed job", got, err)
			}
			missing := j
			missing.SourceSpoolPath = ""
			if err = validateJob(missing); err == nil {
				t.Fatal("nonterminal accepted without source path")
			}
			if status == StatusPublishing || status == StatusHeld {
				missing = j
				missing.ResultPath = ""
				if err = validateJob(missing); err == nil {
					t.Fatal("nonterminal accepted without result path")
				}
			}
		})
	}
}

func TestReleaseSpoolFreesByteAdmissionButKeepsHistoricalJobLimit(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{MaxSpoolBytes: 12, MaxJobs: 2})
	j := completedForRelease(t, s)
	if _, _, err := s.Enqueue(inputFor("another"), strings.NewReader("another")); !errors.Is(err, ErrLimit) {
		t.Fatal("spool should be full", err)
	}
	if _, err := s.ReleaseSpool(j.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enqueue(inputFor("another"), strings.NewReader("another")); err != nil {
		t.Fatal("released bytes did not free admission", err)
	}
	s = openTest(t, t.TempDir(), Options{MaxJobs: 1})
	j = completedForRelease(t, s)
	if _, err := s.ReleaseSpool(j.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Enqueue(inputFor("another"), strings.NewReader("another")); !errors.Is(err, ErrLimit) {
		t.Fatal("release removed historical ID from MaxJobs accounting", err)
	}
}

func TestReleaseSpoolUncertainCommitDoesNotDeleteBlobs(t *testing.T) {
	for _, failure := range []string{"rename", "directory_sync"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			s := openTest(t, dir, Options{})
			j := completedForRelease(t, s)
			injected := errors.New("uncertain release commit")
			if failure == "rename" {
				s.replaceSnapshot = func(from, to string) error {
					if err := atomicReplace(from, to); err != nil {
						return err
					}
					return injected
				}
			} else {
				s.syncSnapshotDir = func(string) error { return injected }
			}
			s.removeSpoolFile = func(string) error { t.Error("uncertain commit attempted blob deletion"); return nil }
			if _, err := s.ReleaseSpool(j.ID); !errors.Is(err, injected) || !s.poisoned {
				t.Fatal("uncertain release not blocked", err)
			}
			for _, path := range []string{j.SourceSpoolPath, j.ResultPath} {
				if _, err := os.Stat(path); err != nil {
					t.Fatal("uncertain commit deleted blob", err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openTest(t, dir, Options{})
			entries, err := os.ReadDir(filepath.Join(dir, "spool"))
			if err != nil || len(entries) != 0 {
				t.Fatal("recovery did not reap unreferenced blobs", entries, err)
			}
			if _, err = s.Complete(j.ID, "worker", j.Lease.Token, resultFor("result"), nil); err != nil {
				t.Fatal("release recovery lost accepted result identity", err)
			}
		})
	}
}

func TestReleaseSpoolRemovalFailureIsAccountedUntilRecovery(t *testing.T) {
	dir := t.TempDir()
	s := openTest(t, dir, Options{MaxSpoolBytes: 12})
	j := completedForRelease(t, s)
	injected := errors.New("blob deletion denied")
	s.removeSpoolFile = func(path string) error {
		if path == j.SourceSpoolPath {
			return injected
		}
		return os.Remove(path)
	}
	released, err := s.ReleaseSpool(j.ID)
	if !errors.Is(err, injected) || released.SourceSpoolPath != "" || released.ResultPath != "" || s.poisoned {
		t.Fatal("cleanup failure result", released, err)
	}
	if s.usedBytes() != j.Size {
		t.Fatal("orphan bytes escaped capacity accounting", s.usedBytes())
	}
	if _, _, err = s.Enqueue(inputFor("another"), strings.NewReader("another")); !errors.Is(err, ErrLimit) {
		t.Fatal("orphan bytes allowed excessive admission", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, Options{MaxSpoolBytes: 12})
	if s.usedBytes() != 0 {
		t.Fatal("recovered orphan still accounted", s.usedBytes())
	}
	if _, _, err = s.Enqueue(inputFor("another"), strings.NewReader("another")); err != nil {
		t.Fatal("orphan cleanup did not free admission", err)
	}
}
