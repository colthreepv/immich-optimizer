package jobqueue

import (
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

func (s *Store) backoff(attempt int) time.Duration {
	d := s.options.RetryBase
	for n := 1; n < attempt && d < s.options.RetryMax; n++ {
		if d > s.options.RetryMax/2 {
			return s.options.RetryMax
		}
		d *= 2
	}
	if d > s.options.RetryMax {
		return s.options.RetryMax
	}
	return d
}

func validWorker(worker string) bool { return worker != "" && len(worker) <= 512 }

// Claim grants a fresh random token. Expired attempts are first durably moved
// into retry backoff, so repeated worker deaths cannot become a hot retry loop.
func (s *Store) Claim(workerID string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	if !validWorker(workerID) {
		return Job{}, fmt.Errorf("worker ID required")
	}
	now := s.now()
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var candidate *Job
	for _, id := range ids {
		j := s.jobs[id]
		if j.Status == StatusLeased && !now.Before(j.Lease.ExpiresAt) {
			j.Status = StatusQueued
			if j.Attempts >= s.options.MaxAttempts {
				j.Status = StatusFailed
			}
			j.LastError = "worker lease expired"
			j.NextAttemptAt = now.Add(s.backoff(j.Attempts))
			j.UpdatedAt = now
			j.Lease = Lease{}
			if err := s.save(j); err != nil {
				return Job{}, err
			}
		}
		if j.Status != StatusQueued || j.OriginalAssetID == "" || now.Before(j.NextAttemptAt) {
			continue
		}
		if candidate == nil || j.CreatedAt.Before(candidate.CreatedAt) || (j.CreatedAt.Equal(candidate.CreatedAt) && j.ID < candidate.ID) {
			copy := j
			candidate = &copy
		}
	}
	if candidate == nil {
		return Job{}, ErrNoJobs
	}
	j := *candidate
	token, err := randomID()
	if err != nil {
		return Job{}, err
	}
	j.Status = StatusLeased
	j.Attempts++
	j.UpdatedAt = now
	j.Lease = Lease{WorkerID: workerID, Token: token, ExpiresAt: now.Add(s.options.LeaseDuration)}
	if err = s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

func (s *Store) leased(id, workerID, token string) (Job, error) {
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if j.Status != StatusLeased || token == "" || j.Lease.Token != token || j.Lease.WorkerID != workerID || !s.now().Before(j.Lease.ExpiresAt) {
		return Job{}, ErrLease
	}
	return j, nil
}

func (s *Store) Heartbeat(id, workerID, token string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, workerID, token)
	if err != nil {
		return Job{}, err
	}
	j.UpdatedAt = s.now()
	j.Lease.ExpiresAt = j.UpdatedAt.Add(s.options.LeaseDuration)
	if err = s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

func (s *Store) OpenSource(id, workerID, token string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, workerID, token)
	if err != nil {
		return nil, err
	}
	return os.Open(j.SourceSpoolPath)
}

func (s *Store) Fail(id, workerID, token, message string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, workerID, token)
	if err != nil {
		return Job{}, err
	}
	j.Status = StatusQueued
	if j.Attempts >= s.options.MaxAttempts {
		j.Status = StatusFailed
	}
	j.UpdatedAt = s.now()
	j.NextAttemptAt = j.UpdatedAt.Add(s.backoff(j.Attempts))
	j.LastError = boundedError(message)
	j.Lease = Lease{}
	if err = s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

// MarkSkipped is a terminal no-gain outcome: the uploaded original is retained.
func (s *Store) MarkSkipped(id, workerID, token, reason string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, err := s.leased(id, workerID, token)
	if err != nil {
		return Job{}, err
	}
	j.Status = StatusSkipped
	j.UpdatedAt = s.now()
	j.LastError = boundedError(reason)
	j.Lease = Lease{}
	if err = s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

func boundedError(message string) string {
	if len(message) > 4096 {
		return message[:4096]
	}
	return message
}

func resultMatches(j Job, r ResultInput) bool {
	return j.OutputSHA1 == r.SHA1 && j.OutputSHA256 == r.SHA256 && j.OutputSize == r.Size && j.OutputFilename == r.Filename
}

func acceptedResult(j Job, workerID, token string, r ResultInput) bool {
	return (j.Status == StatusPublishing || j.Status == StatusHeld || j.Status == StatusComplete) && token != "" && j.Lease.Token == token && j.Lease.WorkerID == workerID && resultMatches(j, r)
}

// Complete durably installs a verified result before starting publication.
// Once accepted, exact retries by the same lease generation are idempotent,
// including after expiry. No other expired or superseded lease can complete.
func (s *Store) Complete(id, workerID, token string, r ResultInput, body io.Reader) (Job, error) {
	var err error
	if r.SHA1, err = normalizeHash(r.SHA1, sha1.Size); err != nil {
		return Job{}, err
	}
	if r.SHA256, err = normalizeHash(r.SHA256, sha256.Size); err != nil {
		return Job{}, err
	}
	if r.Size < 1 || r.Filename == "" || len(r.Filename) > 4096 {
		return Job{}, fmt.Errorf("invalid result metadata")
	}
	s.mu.Lock()
	if err = s.available(); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	if j, ok := s.jobs[id]; ok && acceptedResult(j, workerID, token, r) {
		s.mu.Unlock()
		return clone(j), nil
	}
	if _, err = s.leased(id, workerID, token); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	if body == nil {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("result body required")
	}
	if r.Size > s.options.MaxResultBytes || r.Size > s.options.MaxSpoolBytes-s.usedBytes()-s.reservedBytes {
		s.mu.Unlock()
		return Job{}, ErrLimit
	}
	s.reservedBytes += r.Size
	s.transfers.Add(1)
	s.mu.Unlock()
	path, err := s.writeSpool(body, r.Size, r.SHA1, r.SHA256)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.transfers.Done()
	s.reservedBytes -= r.Size
	if err != nil {
		return Job{}, err
	}
	if err = s.available(); err != nil {
		_ = os.Remove(path)
		return Job{}, err
	}
	if j, ok := s.jobs[id]; ok && acceptedResult(j, workerID, token, r) {
		_ = os.Remove(path)
		return clone(j), nil
	}
	j, err := s.leased(id, workerID, token)
	if err != nil {
		_ = os.Remove(path)
		return Job{}, err
	}
	j.Status = StatusPublishing
	j.PublicationStage = StagePending
	j.ResultPath = path
	j.OutputSHA1 = r.SHA1
	j.OutputSHA256 = r.SHA256
	j.OutputSize = r.Size
	j.OutputFilename = r.Filename
	j.LastError = ""
	j.UpdatedAt = s.now()
	j.NextAttemptAt = j.UpdatedAt
	if err = s.save(j); err != nil {
		if !s.poisoned {
			_ = os.Remove(path)
		}
		return Job{}, err
	}
	return clone(j), nil
}

// NextPublication must be consumed by a single publisher goroutine. Worker
// leases cover conversion only; publication is owned by the queue's process.
func (s *Store) NextPublication() (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	var best *Job
	for _, j := range s.jobs {
		if j.Status != StatusPublishing || s.now().Before(j.NextAttemptAt) {
			continue
		}
		if best == nil || j.CreatedAt.Before(best.CreatedAt) || (j.CreatedAt.Equal(best.CreatedAt) && j.ID < best.ID) {
			copy := j
			best = &copy
		}
	}
	if best == nil {
		return Job{}, ErrNoJobs
	}
	return clone(*best), nil
}

var stageOrder = map[PublicationStage]int{StagePending: 0, StageUploaded: 1, StageVerified: 2, StageMetadataMigrated: 3, StageOriginalTrashed: 4, StageComplete: 5}

// AdvancePublication persists one checkpoint after the caller confirms its
// external action. Actions must themselves be idempotent/reconciled: a process
// can die after the action succeeds but before its checkpoint is durable.
func (s *Store) AdvancePublication(id string, expected, next PublicationStage, resultAssetID string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	n, validNext := stageOrder[next]
	e, validExpected := stageOrder[expected]
	if !validNext || !validExpected || n != e+1 {
		return Job{}, ErrConflict
	}
	if j.PublicationStage == next && (j.Status == StatusPublishing || j.Status == StatusComplete) && (resultAssetID == "" || j.ResultAssetID == resultAssetID) {
		return clone(j), nil
	}
	if j.Status != StatusPublishing || j.PublicationStage != expected {
		return Job{}, ErrConflict
	}
	if next == StageUploaded {
		if resultAssetID == "" || resultAssetID == j.OriginalAssetID || len(resultAssetID) > 512 {
			return Job{}, fmt.Errorf("distinct result asset ID required")
		}
		j.ResultAssetID = resultAssetID
	} else if resultAssetID != "" && resultAssetID != j.ResultAssetID {
		return Job{}, ErrConflict
	}
	if j.ResultAssetID == "" {
		return Job{}, ErrConflict
	}
	j.PublicationStage = next
	j.UpdatedAt = s.now()
	j.NextAttemptAt = j.UpdatedAt
	j.LastError = ""
	if next == StageComplete {
		j.Status = StatusComplete
	}
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

// Publication retries never drop a result or its checkpoint. Even exhausted
// conversion retry budgets must not strand an already-published replacement.
func (s *Store) RetryPublication(id, message string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if j.Status != StatusPublishing {
		return Job{}, ErrConflict
	}
	j.PublicationAttempts++
	if j.PublicationAttempts >= s.options.MaxAttempts {
		j.Status = StatusHeld
	}
	j.LastError = boundedError(message)
	j.UpdatedAt = s.now()
	j.NextAttemptAt = j.UpdatedAt.Add(s.backoff(j.PublicationAttempts))
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

// HoldPublication durably stops all automatic publication work at the current
// checkpoint. A human or reconciliation routine must explicitly resume it.
func (s *Store) HoldPublication(id, reason string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if j.Status != StatusPublishing && j.Status != StatusHeld {
		return Job{}, ErrConflict
	}
	j.Status = StatusHeld
	j.LastError = boundedError(reason)
	j.UpdatedAt = s.now()
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}

func (s *Store) ResumePublication(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if j.Status != StatusHeld {
		return Job{}, ErrConflict
	}
	j.Status = StatusPublishing
	j.UpdatedAt = s.now()
	j.NextAttemptAt = j.UpdatedAt
	j.PublicationAttempts = 0
	j.LastError = ""
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}
