package jobqueue

// MarkSyncRepublished records a successful remote sync event after the caller
// commits the alias ledger. The caller must wait for that HTTP response first.
// This checkpoint does not advance the publication stage. Repeated calls after
// acceptance return the recorded job without rewriting it, including later stages.
func (s *Store) MarkSyncRepublished(id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.available(); err != nil {
		return Job{}, err
	}
	j, ok := s.jobs[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if j.SyncRepublished {
		return clone(j), nil
	}
	if j.Status != StatusPublishing || j.PublicationStage != StageMetadataMigrated || j.ResultAssetID == "" {
		return Job{}, ErrConflict
	}
	j.SyncRepublished = true
	j.UpdatedAt = s.now()
	if err := s.save(j); err != nil {
		return Job{}, err
	}
	return clone(j), nil
}
