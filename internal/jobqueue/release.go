package jobqueue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func terminal(status Status) bool {
	return status == StatusComplete || status == StatusSkipped || status == StatusFailed
}

// ReleaseSpool removes local bytes for a terminal job. The caller must first
// verify the remote asset it intends to keep; this store makes no remote calls.
// Hashes, sizes, accepted lease identity and the deduplication record remain.
// MaxJobs still counts this historical ID after its spool bytes are released.
func (s *Store) ReleaseSpool(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if !terminal(j.Status) {
		return Job{}, ErrConflict
	}
	if j.SourceSpoolPath == "" && j.ResultPath == "" {
		return clone(j), nil
	}
	blobs := []struct {
		path string
		size int64
	}{{j.SourceSpoolPath, j.Size}, {j.ResultPath, j.OutputSize}}
	for _, blob := range blobs {
		if blob.path != "" && (filepath.Dir(blob.path) != filepath.Join(s.dir, "spool") || !strings.HasPrefix(filepath.Base(blob.path), "blob-")) {
			return Job{}, fmt.Errorf("refusing to release unowned spool path")
		}
	}
	j.SourceSpoolPath = ""
	j.ResultPath = ""
	j.UpdatedAt = s.now()
	// Even an error from rename can mean the cleared paths committed. Never
	// delete a blob until the complete snapshot commit, including fsync, succeeds.
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	var cleanupErr error
	for _, blob := range blobs {
		if blob.path == "" {
			continue
		}
		if err := s.removeSpoolFile(blob.path); err != nil && !os.IsNotExist(err) {
			// These bytes still occupy disk despite the cleared paths. Keep them
			// in admission accounting until reopen retries orphan cleanup.
			s.orphanBytes += blob.size
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove released spool blob %s: %w", filepath.Base(blob.path), err))
		}
	}
	if err := syncDirectory(filepath.Join(s.dir, "spool")); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("sync released spool directory: %w", err))
	}
	if cleanupErr != nil {
		return clone(j), fmt.Errorf("spool paths cleared; reopen queue to retry orphan cleanup: %w", cleanupErr)
	}
	return clone(j), nil
}
