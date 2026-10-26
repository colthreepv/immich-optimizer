package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorePersistsAndReopens(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	record := testRecord("owner-a", strings.Repeat("a", 40), "asset-a")
	if err := store.Put(record); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() after Put error = %v", err)
	}

	got, ok := reopened.FindSource(record.OwnerID, record.SourceSHA1)
	if !ok {
		t.Fatal("FindSource() did not find persisted record")
	}
	if !reflect.DeepEqual(got, record) {
		t.Fatalf("persisted record = %#v, want %#v", got, record)
	}
}

func TestStoreAliasLookupAndCanonicalHashes(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	record := testRecord("owner-a", strings.Repeat("a", 40), "asset-a")
	record.SourceSHA1 = strings.ToUpper(record.SourceSHA1)
	record.OutputSHA1 = " " + strings.ToUpper(record.OutputSHA1) + " "
	if err := store.Put(record); err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	bySource, ok := store.FindSource("owner-a", strings.ToUpper(strings.Repeat("a", 40)))
	if !ok {
		t.Fatal("FindSource() alias lookup failed")
	}
	if bySource.SourceSHA1 != strings.Repeat("a", 40) || bySource.OutputSHA1 != strings.Repeat("b", 40) {
		t.Fatalf("hashes were not canonicalized: %#v", bySource)
	}

	byAsset, ok := store.FindAsset("owner-a", "asset-a")
	if !ok {
		t.Fatal("FindAsset() alias lookup failed")
	}
	if !reflect.DeepEqual(byAsset, bySource) {
		t.Fatalf("FindAsset() = %#v, want %#v", byAsset, bySource)
	}
}

func TestStoreKeepsOwnersIsolated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	source := strings.Repeat("c", 40)
	first := testRecord("owner-a", source, "asset-a")
	second := testRecord("owner-b", source, "asset-b")
	if err := store.Put(first); err != nil {
		t.Fatalf("Put(first) error = %v", err)
	}
	if err := store.Put(second); err != nil {
		t.Fatalf("Put(second) error = %v", err)
	}

	first.OutputSHA1 = strings.Repeat("d", 40)
	first.UpdatedAt = first.UpdatedAt.Add(time.Minute)
	if err := store.Put(first); err != nil {
		t.Fatalf("Put(first update) error = %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() after updates error = %v", err)
	}
	records := reopened.Records()
	if len(records) != 2 {
		t.Fatalf("Records() length = %d, want 2", len(records))
	}

	gotFirst, ok := reopened.FindSource("owner-a", source)
	if !ok || gotFirst.OutputSHA1 != first.OutputSHA1 {
		t.Fatalf("owner-a record = %#v, %v", gotFirst, ok)
	}
	gotSecond, ok := reopened.FindSource("owner-b", source)
	if !ok || !reflect.DeepEqual(gotSecond, second) {
		t.Fatalf("owner-b record = %#v, %v", gotSecond, ok)
	}
	if _, ok := reopened.FindAsset("owner-a", "asset-b"); ok {
		t.Fatal("FindAsset() crossed owner boundary")
	}
}

func TestOpenCorruptStateReturnsErrorAndPreservesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ledgerFilename)
	corrupt := []byte(`{"version":1,"records":[}`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}

	if _, err := Open(dir); err == nil {
		t.Fatal("Open() error = nil, want corruption error")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read preserved state: %v", err)
	}
	if !reflect.DeepEqual(got, corrupt) {
		t.Fatalf("corrupt state changed: got %q, want %q", got, corrupt)
	}
}

func TestStoreConcurrentUpdatesArePersisted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	const count = 24
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for index := 0; index < count; index++ {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			source := fmt.Sprintf("%040x", index+1)
			errs <- store.Put(testRecord("owner-a", source, fmt.Sprintf("asset-%d", index)))
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put() error = %v", err)
		}
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() after concurrent updates error = %v", err)
	}
	if got := len(reopened.Records()); got != count {
		t.Fatalf("persisted record count = %d, want %d", got, count)
	}
}

func TestStorePutRollsBackWhenPersistenceFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	original := testRecord("owner-a", strings.Repeat("e", 40), "asset-original")
	if err := store.Put(original); err != nil {
		t.Fatalf("Put(original) error = %v", err)
	}

	originalPersist := store.persist
	store.persist = func(map[sourceKey]Record) error {
		return errors.New("injected persistence failure")
	}

	update := original
	update.OutputSHA1 = strings.Repeat("f", 40)
	update.AssetID = "asset-update"
	update.UpdatedAt = update.UpdatedAt.Add(time.Minute)
	if err := store.Put(update); err == nil {
		t.Fatal("Put(update) error = nil, want persistence failure")
	}

	insert := testRecord("owner-a", strings.Repeat("1", 40), "asset-insert")
	if err := store.Put(insert); err == nil {
		t.Fatal("Put(insert) error = nil, want persistence failure")
	}

	got, ok := store.FindSource(original.OwnerID, original.SourceSHA1)
	if !ok || !reflect.DeepEqual(got, original) {
		t.Fatalf("FindSource() after rollback = %#v, %v", got, ok)
	}
	if _, ok := store.FindSource(insert.OwnerID, insert.SourceSHA1); ok {
		t.Fatal("failed insert remained in memory")
	}

	store.persist = originalPersist
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() after persistence failures error = %v", err)
	}
	records := reopened.Records()
	if len(records) != 1 || !reflect.DeepEqual(records[0], original) {
		t.Fatalf("persisted records after failures = %#v, want original only", records)
	}
}

func TestStoreValidatesRecords(t *testing.T) {
	t.Parallel()

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	valid := testRecord("owner-a", strings.Repeat("2", 40), "")
	valid.State = stateStaged
	valid.OutputSHA1 = ""
	if err := store.Put(valid); err != nil {
		t.Fatalf("Put(staged without asset/output) error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Record)
	}{
		{name: "owner", mutate: func(record *Record) { record.OwnerID = " " }},
		{name: "source", mutate: func(record *Record) { record.SourceSHA1 = "" }},
		{name: "source hex", mutate: func(record *Record) { record.SourceSHA1 = strings.Repeat("z", 40) }},
		{name: "output hex", mutate: func(record *Record) { record.OutputSHA1 = strings.Repeat("z", 40) }},
		{name: "state", mutate: func(record *Record) { record.State = "unknown" }},
		{name: "verified asset", mutate: func(record *Record) { record.State = stateVerified; record.AssetID = "" }},
		{name: "verified output", mutate: func(record *Record) { record.State = stateVerified; record.OutputSHA1 = "" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := testRecord("owner-a", strings.Repeat("3", 40), "asset-a")
			test.mutate(&record)
			if err := store.Put(record); err == nil {
				t.Fatal("Put() error = nil, want validation error")
			}
		})
	}
}

func testRecord(ownerID, sourceSHA1, assetID string) Record {
	return Record{
		OwnerID:        ownerID,
		AssetID:        assetID,
		SourceSHA1:     sourceSHA1,
		OutputSHA1:     strings.Repeat("b", 40),
		SourceFilename: "original.jpg",
		OutputFilename: "optimized.jpg",
		Preset:         "balanced",
		State:          stateVerified,
		SpoolPath:      "/spool/original.jpg",
		SourceSize:     2048,
		OutputSize:     1024,
		UpdatedAt:      time.Date(2026, time.October, 26, 8, 30, 0, 0, time.UTC),
	}
}
