package jobqueue

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMarkSyncRepublishedGuardsAndDurableIdempotence(t *testing.T) {
	dir := t.TempDir()
	clock := newClock()
	s := openTest(t, dir, Options{Now: clock.now})
	if _, err := s.MarkSyncRepublished("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	j := enqueueTest(t, s, "source")
	assertBlocked := func() {
		t.Helper()
		if _, err := s.MarkSyncRepublished(j.ID); !errors.Is(err, ErrConflict) {
			t.Fatal("invalid sync checkpoint accepted", j.Status, j.PublicationStage, err)
		}
	}
	assertBlocked()
	j = claimTest(t, s, "worker")
	assertBlocked()
	j = completeTest(t, s, j)
	assertBlocked()
	for _, next := range []PublicationStage{StageUploaded, StageVerified, StageMetadataMigrated} {
		var err error
		j, err = s.AdvancePublication(j.ID, j.PublicationStage, next, "replacement")
		if err != nil {
			t.Fatal(err)
		}
		if next != StageMetadataMigrated {
			assertBlocked()
		}
	}
	var err error
	j, err = s.HoldPublication(j.ID, "manual hold")
	if err != nil {
		t.Fatal(err)
	}
	assertBlocked()
	j, err = s.ResumePublication(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A failed save cannot acknowledge a sync event in memory.
	s.snapshotLimit = 1
	if _, err = s.MarkSyncRepublished(j.ID); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if got, err := s.Get(j.ID); err != nil || got.SyncRepublished {
		t.Fatal("failed save acknowledged sync event", got, err)
	}
	s.snapshotLimit = maxSnapshotBytes
	clock.advance(time.Second)
	marked, err := s.MarkSyncRepublished(j.ID)
	if err != nil || !marked.SyncRepublished || marked.PublicationStage != StageMetadataMigrated {
		t.Fatal(marked, err)
	}
	want := j
	want.SyncRepublished = true
	want.UpdatedAt = marked.UpdatedAt
	if !reflect.DeepEqual(marked, want) {
		t.Fatal("sync checkpoint changed other state", marked, want)
	}
	clock.advance(time.Second)
	if again, err := s.MarkSyncRepublished(j.ID); err != nil || !reflect.DeepEqual(again, marked) {
		t.Fatal("checkpoint was not idempotent", again, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir, Options{Now: clock.now})
	if got, err := s.Get(j.ID); err != nil || !reflect.DeepEqual(got, marked) {
		t.Fatal("sync checkpoint lost on reopen", got, err)
	}
	for _, next := range []PublicationStage{StageOriginalTrashed, StageComplete} {
		marked, err = s.AdvancePublication(j.ID, marked.PublicationStage, next, "replacement")
		if err != nil {
			t.Fatal(err)
		}
	}
	marked, err = s.ReleaseSpool(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.MarkSyncRepublished(j.ID); err != nil || !reflect.DeepEqual(again, marked) {
		t.Fatal("later-stage retry lost sync checkpoint", again, err)
	}
}

func TestMarkSyncRepublishedRequiresKnownResultAsset(t *testing.T) {
	s := openTest(t, t.TempDir(), Options{})
	enqueueTest(t, s, "source")
	j := completeTest(t, s, claimTest(t, s, "worker"))
	// Simulate an incomplete internal checkpoint to check this independent guard.
	j.PublicationStage = StageMetadataMigrated
	j.ResultAssetID = ""
	s.jobs[j.ID] = j
	if _, err := s.MarkSyncRepublished(j.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("sync event accepted without output ID", err)
	}
}
