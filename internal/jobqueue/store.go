package jobqueue

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const stateVersion = 1
const maxSnapshotBytes = 128 << 20

type snapshot struct {
	Version int            `json:"version"`
	Jobs    map[string]Job `json:"jobs"`
}

// Store is safe for concurrent goroutines, and holds an exclusive OS process
// lock until Close. Keep dir on a local durable filesystem (not NFS/SMB).
type Store struct {
	mu            sync.Mutex
	dir           string
	options       Options
	lock          *os.File
	jobs          map[string]Job
	closed        bool
	poisoned      bool
	reservedBytes int64
	orphanBytes   int64
	reservedJobs  int
	transfers     sync.WaitGroup
	// Per-store persistence operations also allow small, deterministic fault tests.
	snapshotLimit   int64
	replaceSnapshot func(string, string) error
	syncSnapshotDir func(string) error
	removeSpoolFile func(string) error
}

func Open(dir string, options Options) (*Store, error) {
	if options.MaxJobs == 0 {
		options.MaxJobs = 10000
	}
	if options.MaxSpoolBytes == 0 {
		options.MaxSpoolBytes = 100 << 30
	}
	if options.MaxSourceBytes == 0 {
		options.MaxSourceBytes = 10 << 30
	}
	if options.MaxResultBytes == 0 {
		options.MaxResultBytes = 10 << 30
	}
	if options.LeaseDuration == 0 {
		options.LeaseDuration = 5 * time.Minute
	}
	if options.RetryBase == 0 {
		options.RetryBase = 30 * time.Second
	}
	if options.RetryMax == 0 {
		options.RetryMax = time.Hour
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 10
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.MaxJobs < 1 || options.MaxSpoolBytes < 1 || options.MaxSourceBytes < 1 || options.MaxResultBytes < 1 || options.LeaseDuration <= 0 || options.RetryBase <= 0 || options.RetryMax < options.RetryBase || options.MaxAttempts < 1 {
		return nil, fmt.Errorf("invalid queue options")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = mkdirDurable(filepath.Join(abs, "spool")); err != nil {
		return nil, err
	}
	lock, err := lockProcess(filepath.Join(abs, "queue.lock"))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: abs, options: options, lock: lock, jobs: map[string]Job{}, snapshotLimit: maxSnapshotBytes, replaceSnapshot: atomicReplace, syncSnapshotDir: syncDirectory, removeSpoolFile: os.Remove}
	if err = s.recover(); err != nil {
		_ = unlockProcess(lock)
		return nil, err
	}
	return s, nil
}

func (s *Store) now() time.Time { return s.options.Now().UTC() }

// Sync newly-created directory entries all the way to the first preexisting
// ancestor; syncing only the queue directory misses its entry in its parent.
func mkdirDurable(path string) error {
	var created []string
	for p := path; ; p = filepath.Dir(p) {
		_, err := os.Stat(p)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		created = append(created, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	for _, p := range created {
		if err := syncDirectory(p); err != nil {
			return err
		}
		if err := syncDirectory(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) available() error {
	if s.closed || s.poisoned {
		return ErrClosed
	}
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	// Do not unlock while a streaming upload still owns a temporary spool file.
	s.transfers.Wait()
	return unlockProcess(s.lock)
}

func clone(j Job) Job {
	if j.UploadFields != nil {
		fields := make(map[string][]string, len(j.UploadFields))
		for k, v := range j.UploadFields {
			fields[k] = append([]string(nil), v...)
		}
		j.UploadFields = fields
	}
	return j
}

func (s *Store) Get(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	return clone(j), nil
}

func (s *Store) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, clone(j))
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	return jobs
}

// FindIntent includes terminal jobs so repeat ingestions never create another
// conversion job for an owner/source pair. Only this store's jobs are indexed;
// legacy import ledgers are deliberately outside this package.
func (s *Store) FindIntent(ownerID, sourceSHA256 string) (Job, bool, error) {
	hash, err := normalizeHash(sourceSHA256, sha256.Size)
	if err != nil {
		return Job{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = s.available(); err != nil {
		return Job{}, false, err
	}
	j, ok := s.duplicate(Input{OwnerID: ownerID, SourceSHA256: hash})
	return clone(j), ok, nil
}

func sourceKey(j Input) string { return j.OwnerID + "\x00" + j.SourceSHA256 }
func (s *Store) duplicate(in Input) (Job, bool) {
	for _, j := range s.jobs {
		if sourceKey(j.Input) == sourceKey(in) {
			return j, true
		}
	}
	return Job{}, false
}

func normalizeHash(h string, size int) (string, error) {
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != size {
		return "", fmt.Errorf("invalid %d-byte hex digest", size)
	}
	return strings.ToLower(h), nil
}

func validateInput(in Input) (Input, error) {
	if in.OwnerID == "" || len(in.OwnerID) > 512 || strings.ContainsRune(in.OwnerID, 0) || in.Filename == "" || len(in.Filename) > 4096 || len(in.Preset) > 4096 || len(in.SourcePath) > 8192 || len(in.OriginalAssetID) > 512 || in.Size < 1 {
		return in, fmt.Errorf("invalid input metadata")
	}
	var err error
	if in.SourceSHA1, err = normalizeHash(in.SourceSHA1, sha1.Size); err != nil {
		return in, err
	}
	if in.SourceSHA256, err = normalizeHash(in.SourceSHA256, sha256.Size); err != nil {
		return in, err
	}
	b, err := json.Marshal(in.UploadFields)
	if err != nil || len(b) > 65536 {
		return in, fmt.Errorf("upload fields exceed 64 KiB")
	}
	return in, nil
}

func (s *Store) usedBytes() int64 {
	n := s.orphanBytes
	for _, j := range s.jobs {
		if j.SourceSpoolPath != "" {
			n += j.Size
		}
		if j.ResultPath != "" {
			n += j.OutputSize
		}
	}
	return n
}

func randomID() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Enqueue returns an existing job for the same owner and source SHA256. An empty
// OriginalAssetID creates an ingestion intent that cannot be claimed yet.
func (s *Store) Enqueue(in Input, source io.Reader) (Job, bool, error) {
	var err error
	if in, err = validateInput(in); err != nil {
		return Job{}, false, err
	}
	in = clone(Job{Input: in}).Input
	s.mu.Lock()
	if err = s.available(); err != nil {
		s.mu.Unlock()
		return Job{}, false, err
	}
	if j, ok := s.duplicate(in); ok {
		s.mu.Unlock()
		return clone(j), false, nil
	}
	if source == nil {
		s.mu.Unlock()
		return Job{}, false, fmt.Errorf("source reader required")
	}
	if len(s.jobs)+s.reservedJobs >= s.options.MaxJobs || in.Size > s.options.MaxSourceBytes || in.Size > s.options.MaxSpoolBytes-s.usedBytes()-s.reservedBytes {
		s.mu.Unlock()
		return Job{}, false, ErrLimit
	}
	s.reservedBytes += in.Size
	s.reservedJobs++
	s.transfers.Add(1)
	s.mu.Unlock()
	path, err := s.writeSpool(source, in.Size, in.SourceSHA1, in.SourceSHA256)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.transfers.Done()
	s.reservedBytes -= in.Size
	s.reservedJobs--
	if err != nil {
		return Job{}, false, err
	}
	if err = s.available(); err != nil {
		_ = os.Remove(path)
		return Job{}, false, err
	}
	if j, ok := s.duplicate(in); ok {
		_ = os.Remove(path)
		return clone(j), false, nil
	}
	id, err := randomID()
	if err != nil {
		_ = os.Remove(path)
		return Job{}, false, err
	}
	now := s.now()
	status := StatusQueued
	if in.OriginalAssetID == "" {
		status = StatusPublishingOriginal
	}
	j := Job{Input: in, ID: id, Status: status, SourceSpoolPath: path, CreatedAt: now, UpdatedAt: now, NextAttemptAt: now, PublicationStage: StagePending}
	if err = s.save(j); err != nil {
		// Before rename the snapshot cannot refer to this new blob. Once rename
		// is attempted its outcome may be uncertain, so recovery must decide.
		if !s.poisoned {
			_ = os.Remove(path)
		}
		return Job{}, false, err
	}
	return clone(j), true, nil
}

func (s *Store) MarkOriginalUploaded(id, assetID string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if assetID == "" || len(assetID) > 512 {
		return Job{}, fmt.Errorf("original asset ID required")
	}
	if j.OriginalAssetID == assetID {
		return clone(j), nil
	}
	if j.Status != StatusPublishingOriginal || j.OriginalAssetID != "" {
		return Job{}, ErrConflict
	}
	j.OriginalAssetID = assetID
	j.Status = StatusQueued
	j.UpdatedAt = s.now()
	j.NextAttemptAt = j.UpdatedAt
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

// writeSpool never incorporates caller-controlled paths, bounds reads (including
// one byte to detect oversize bodies), and syncs bytes before publishing a path.
func (s *Store) writeSpool(r io.Reader, size int64, wantSHA1, wantSHA256 string) (path string, err error) {
	f, err := os.CreateTemp(filepath.Join(s.dir, "spool"), "blob-")
	if err != nil {
		return "", err
	}
	path = f.Name()
	ownedPath := path
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(ownedPath)
		}
	}()
	h1, h256 := sha1.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h1, h256), io.LimitReader(r, size))
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("size mismatch: got %d, expected %d", n, size)
	}
	var extra [1]byte
	if n, readErr := io.ReadFull(r, extra[:]); n != 0 || (readErr != io.EOF && readErr != io.ErrUnexpectedEOF) {
		return "", fmt.Errorf("body exceeds declared size or read failed")
	}
	if hex.EncodeToString(h1.Sum(nil)) != wantSHA1 || hex.EncodeToString(h256.Sum(nil)) != wantSHA256 {
		return "", fmt.Errorf("content hash mismatch")
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = syncDirectory(filepath.Join(s.dir, "spool")); err != nil {
		return "", err
	}
	return path, nil
}

// save changes memory only after the atomic snapshot rename. An uncertain I/O
// outcome poisons the store until reopen; subsequent writes cannot roll back a
// successfully renamed state with an older in-memory snapshot.
func (s *Store) save(j Job) error {
	jobs := make(map[string]Job, len(s.jobs)+1)
	for k, v := range s.jobs {
		jobs[k] = v
	}
	jobs[j.ID] = j
	// Bound encoding allocations before marshaling the entire snapshot. Each
	// individual job is bounded by input validation; the whole map may be large.
	encodedBytes := int64(64)
	for id, job := range jobs {
		encoded, encodeErr := json.Marshal(job)
		if encodeErr != nil {
			return encodeErr
		}
		encodedBytes += int64(len(encoded) + len(id) + 4)
		if encodedBytes > s.snapshotLimit {
			return ErrLimit
		}
	}
	b, err := json.Marshal(snapshot{Version: stateVersion, Jobs: jobs})
	if err != nil {
		return err
	}
	if int64(len(b)) > s.snapshotLimit {
		return ErrLimit
	}
	f, err := os.CreateTemp(s.dir, "queue-tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = s.replaceSnapshot(tmp, filepath.Join(s.dir, "queue.json")); err != nil {
		s.poisoned = true
		return err
	}
	s.jobs = jobs
	if err = s.syncSnapshotDir(s.dir); err != nil {
		s.poisoned = true
		return err
	}
	return nil
}

func (s *Store) recover() error {
	f, err := os.Open(filepath.Join(s.dir, "queue.json"))
	if err == nil {
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil {
			return statErr
		}
		if info.Size() > maxSnapshotBytes {
			return fmt.Errorf("queue snapshot exceeds size limit")
		}
		var state snapshot
		decoder := json.NewDecoder(io.LimitReader(f, maxSnapshotBytes+1))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&state); err != nil {
			return fmt.Errorf("recover queue: %w", err)
		}
		var trailing any
		if err = decoder.Decode(&trailing); err != io.EOF {
			return fmt.Errorf("trailing queue data")
		}
		if state.Version != stateVersion || state.Jobs == nil {
			return fmt.Errorf("unsupported or invalid queue snapshot")
		}
		s.jobs = state.Jobs
	} else if !os.IsNotExist(err) {
		return err
	}
	refs := map[string]bool{}
	keys := map[string]bool{}
	for id, j := range s.jobs {
		if id != j.ID || len(id) != 48 {
			return fmt.Errorf("invalid job ID in snapshot")
		}
		if _, err = validateInput(j.Input); err != nil {
			return fmt.Errorf("invalid persisted input: %w", err)
		}
		key := sourceKey(j.Input)
		if keys[key] {
			return fmt.Errorf("duplicate owner/source in snapshot")
		}
		keys[key] = true
		if err = validateJob(j); err != nil {
			return err
		}
		for _, entry := range []struct {
			path string
			size int64
		}{{j.SourceSpoolPath, j.Size}, {j.ResultPath, j.OutputSize}} {
			if entry.path == "" {
				continue
			}
			if filepath.Dir(entry.path) != filepath.Join(s.dir, "spool") || !strings.HasPrefix(filepath.Base(entry.path), "blob-") || refs[entry.path] {
				return fmt.Errorf("invalid or shared spool path")
			}
			info, e := os.Lstat(entry.path)
			if e != nil {
				return fmt.Errorf("missing spool blob: %w", e)
			}
			if !info.Mode().IsRegular() || info.Size() != entry.size {
				return fmt.Errorf("invalid spool blob")
			}
			refs[entry.path] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, "spool"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(s.dir, "spool", entry.Name())
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "blob-") && !refs[path] {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
	}
	temps, err := filepath.Glob(filepath.Join(s.dir, "queue-tmp-*"))
	if err != nil {
		return err
	}
	for _, p := range temps {
		if err = os.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

func validateJob(j Job) error {
	switch j.Status {
	case StatusPublishingOriginal:
		if j.OriginalAssetID != "" {
			return fmt.Errorf("invalid ingestion intent")
		}
	case StatusQueued, StatusLeased, StatusPublishing, StatusHeld, StatusComplete, StatusFailed, StatusSkipped:
		if j.OriginalAssetID == "" {
			return fmt.Errorf("missing original asset ID")
		}
	default:
		return fmt.Errorf("invalid job status")
	}
	if j.SourceSpoolPath == "" && !terminal(j.Status) {
		return fmt.Errorf("missing source path")
	}
	if j.Status == StatusLeased && (j.Lease.Token == "" || j.Lease.WorkerID == "" || j.Lease.ExpiresAt.IsZero()) {
		return fmt.Errorf("invalid persisted lease")
	}
	if j.Status == StatusPublishing || j.Status == StatusHeld || j.Status == StatusComplete {
		if (j.ResultPath == "" && !terminal(j.Status)) || j.OutputSize < 1 {
			return fmt.Errorf("missing result blob")
		}
		if _, err := normalizeHash(j.OutputSHA1, sha1.Size); err != nil {
			return err
		}
		if _, err := normalizeHash(j.OutputSHA256, sha256.Size); err != nil {
			return err
		}
	}
	stage, ok := stageOrder[j.PublicationStage]
	if !ok {
		return fmt.Errorf("invalid publication stage")
	}
	if (stage > 0 && (j.ResultAssetID == "" || j.ResultAssetID == j.OriginalAssetID)) || (j.Status == StatusComplete && j.PublicationStage != StageComplete) || (j.Status != StatusComplete && j.PublicationStage == StageComplete) {
		return fmt.Errorf("inconsistent publication checkpoint")
	}
	return nil
}
