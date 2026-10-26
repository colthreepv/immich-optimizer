package state

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ledgerFilename = "ledger.json"
	ledgerVersion  = 1
)

const (
	stateStaged   = "staged"
	stateUploaded = "uploaded"
	stateVerified = "verified"
	stateFailed   = "failed"
)

// Record links an original file digest to its processed output.
type Record struct {
	OwnerID        string    `json:"owner_id"`
	AssetID        string    `json:"asset_id"`
	SourceSHA1     string    `json:"source_sha1"`
	OutputSHA1     string    `json:"output_sha1"`
	SourceFilename string    `json:"source_filename"`
	OutputFilename string    `json:"output_filename"`
	Preset         string    `json:"preset"`
	State          string    `json:"state"`
	SpoolPath      string    `json:"spool_path"`
	SourceSize     int64     `json:"source_size"`
	OutputSize     int64     `json:"output_size"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type sourceKey struct {
	ownerID    string
	sourceSHA1 string
}

type ledger struct {
	Version int      `json:"version"`
	Records []Record `json:"records"`
}

// Store is a concurrency-safe persistent hash ledger.
type Store struct {
	mu        sync.RWMutex
	statePath string
	records   map[sourceKey]Record
	persist   func(map[sourceKey]Record) error
}

// Open loads a ledger from dir. A corrupt existing ledger is returned as an
// error and is left in place for inspection or recovery.
func Open(dir string) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("state directory is required")
	}

	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	statePath := filepath.Join(dir, ledgerFilename)
	records, err := loadLedger(statePath)
	if err != nil {
		return nil, err
	}

	store := &Store{
		statePath: statePath,
		records:   records,
	}
	store.persist = func(records map[sourceKey]Record) error {
		return writeLedger(store.statePath, records)
	}

	return store, nil
}

// Put inserts or replaces a record for the same owner and source digest. The
// in-memory change is rolled back when persistence fails.
func (s *Store) Put(record Record) error {
	normalized, err := validateRecord(record)
	if err != nil {
		return err
	}

	key := sourceKey{ownerID: normalized.OwnerID, sourceSHA1: normalized.SourceSHA1}

	s.mu.Lock()
	defer s.mu.Unlock()

	previous, existed := s.records[key]
	s.records[key] = normalized
	if err := s.persist(s.records); err != nil {
		if existed {
			s.records[key] = previous
		} else {
			delete(s.records, key)
		}
		return fmt.Errorf("persist state: %w", err)
	}

	return nil
}

// FindSource finds a record by its owner and original SHA-1 digest.
func (s *Store) FindSource(ownerID, sourceSHA1 string) (Record, bool) {
	normalizedHash, err := normalizeSHA1(sourceSHA1)
	if err != nil || strings.TrimSpace(ownerID) == "" {
		return Record{}, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.records[sourceKey{ownerID: ownerID, sourceSHA1: normalizedHash}]
	return record, ok
}

// FindAsset finds the most recently updated record for an owner and asset.
func (s *Store) FindAsset(ownerID, assetID string) (Record, bool) {
	if strings.TrimSpace(ownerID) == "" || assetID == "" {
		return Record{}, false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var found Record
	var foundKey sourceKey
	ok := false
	for key, record := range s.records {
		if key.ownerID != ownerID || record.AssetID != assetID {
			continue
		}
		if !ok || record.UpdatedAt.After(found.UpdatedAt) ||
			(record.UpdatedAt.Equal(found.UpdatedAt) && key.sourceSHA1 < foundKey.sourceSHA1) {
			found = record
			foundKey = key
			ok = true
		}
	}

	return found, ok
}

// Records returns a stable snapshot ordered by owner and source digest.
func (s *Store) Records() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := recordsFromMap(s.records)
	result := make([]Record, len(records))
	copy(result, records)
	return result
}

func validateRecord(record Record) (Record, error) {
	if strings.TrimSpace(record.OwnerID) == "" {
		return Record{}, errors.New("owner ID is required")
	}

	sourceSHA1, err := normalizeSHA1(record.SourceSHA1)
	if err != nil {
		return Record{}, fmt.Errorf("source SHA-1: %w", err)
	}

	outputSHA1 := ""
	if record.OutputSHA1 != "" {
		outputSHA1, err = normalizeSHA1(record.OutputSHA1)
		if err != nil {
			return Record{}, fmt.Errorf("output SHA-1: %w", err)
		}
	}

	switch record.State {
	case stateStaged, stateFailed:
	case stateUploaded, stateVerified:
		if record.AssetID == "" {
			return Record{}, fmt.Errorf("asset ID is required for %s records", record.State)
		}
		if outputSHA1 == "" {
			return Record{}, fmt.Errorf("output SHA-1 is required for %s records", record.State)
		}
	default:
		return Record{}, fmt.Errorf("invalid state %q", record.State)
	}

	record.SourceSHA1 = sourceSHA1
	record.OutputSHA1 = outputSHA1
	return record, nil
}

func normalizeSHA1(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "", errors.New("digest is required")
	}
	if len(normalized) != sha1.Size*2 {
		return "", fmt.Errorf("must be a %d-character hexadecimal digest", sha1.Size*2)
	}
	if _, err := hex.DecodeString(normalized); err != nil {
		return "", errors.New("must be a hexadecimal digest")
	}
	return normalized, nil
}

func loadLedger(path string) (map[sourceKey]Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[sourceKey]Record), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var stored ledger
	if err := decoder.Decode(&stored); err != nil {
		return nil, fmt.Errorf("decode state %s: %w", path, err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode state %s: trailing JSON value", path)
		}
		return nil, fmt.Errorf("decode state %s: %w", path, err)
	}

	if stored.Version != ledgerVersion {
		return nil, fmt.Errorf("decode state %s: unsupported version %d", path, stored.Version)
	}

	records := make(map[sourceKey]Record, len(stored.Records))
	for index, record := range stored.Records {
		validated, err := validateRecord(record)
		if err != nil {
			return nil, fmt.Errorf("decode state %s: record %d: %w", path, index, err)
		}
		if validated.SourceSHA1 != record.SourceSHA1 || validated.OutputSHA1 != record.OutputSHA1 {
			return nil, fmt.Errorf("decode state %s: record %d: hashes are not lowercase hexadecimal", path, index)
		}

		key := sourceKey{ownerID: record.OwnerID, sourceSHA1: record.SourceSHA1}
		if _, exists := records[key]; exists {
			return nil, fmt.Errorf("decode state %s: duplicate source key for owner %q and hash %q", path, record.OwnerID, record.SourceSHA1)
		}
		records[key] = validated
	}

	return records, nil
}

func writeLedger(path string, records map[sourceKey]Record) error {
	data, err := json.MarshalIndent(ledger{
		Version: ledgerVersion,
		Records: recordsFromMap(records),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set temporary state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	removeTemporary = false

	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func recordsFromMap(records map[sourceKey]Record) []Record {
	result := make([]Record, 0, len(records))
	for _, record := range records {
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].OwnerID != result[j].OwnerID {
			return result[i].OwnerID < result[j].OwnerID
		}
		return result[i].SourceSHA1 < result[j].SourceSHA1
	})
	return result
}
