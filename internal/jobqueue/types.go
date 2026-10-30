// Package jobqueue provides a single-process, durable optimizer queue. It owns
// its spool; caller-supplied filenames and paths are metadata, never file paths.
package jobqueue

import (
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("job not found")
	ErrNoJobs   = errors.New("no eligible jobs")
	ErrLease    = errors.New("invalid or expired lease")
	ErrLimit    = errors.New("queue admission limit exceeded")
	ErrConflict = errors.New("job state conflict")
	ErrLocked   = errors.New("queue is locked by another process")
	ErrClosed   = errors.New("queue is closed or requires recovery")
)

type Status string

const (
	StatusPublishingOriginal Status = "publishing_original"
	StatusQueued             Status = "queued"
	StatusLeased             Status = "leased"
	StatusPublishing         Status = "publishing"
	StatusHeld               Status = "held"
	StatusComplete           Status = "complete"
	StatusFailed             Status = "failed"
	StatusSkipped            Status = "skipped"
)

type PublicationStage string

const (
	StagePending          PublicationStage = "pending"
	StageUploaded         PublicationStage = "uploaded"
	StageVerified         PublicationStage = "verified"
	StageMetadataMigrated PublicationStage = "metadata_migrated"
	StageOriginalTrashed  PublicationStage = "original_trashed"
	StageComplete         PublicationStage = "complete"
)

type Input struct {
	OriginalAssetID string              `json:"originalAssetId"`
	OwnerID         string              `json:"ownerId"`
	SourceSHA1      string              `json:"sourceSha1"`
	SourceSHA256    string              `json:"sourceSha256"`
	Size            int64               `json:"size"`
	Filename        string              `json:"filename"`
	Preset          string              `json:"preset"`
	SourcePath      string              `json:"sourcePath,omitempty"`
	UploadFields    map[string][]string `json:"uploadFields,omitempty"`
}

type Lease struct {
	WorkerID  string    `json:"workerId"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Job struct {
	Input
	ID                  string           `json:"id"`
	Status              Status           `json:"status"`
	SourceSpoolPath     string           `json:"sourceSpoolPath,omitempty"`
	ResultPath          string           `json:"resultPath,omitempty"`
	OutputSHA1          string           `json:"outputSha1,omitempty"`
	OutputSHA256        string           `json:"outputSha256,omitempty"`
	OutputSize          int64            `json:"outputSize,omitempty"`
	OutputFilename      string           `json:"outputFilename,omitempty"`
	ResultAssetID       string           `json:"resultAssetId,omitempty"`
	SyncRepublished     bool             `json:"syncRepublished"`
	Lease               Lease            `json:"lease"`
	Attempts            int              `json:"attempts"`
	NextAttemptAt       time.Time        `json:"nextAttemptAt"`
	PublicationStage    PublicationStage `json:"publicationStage"`
	PublicationAttempts int              `json:"publicationAttempts"`
	LastError           string           `json:"lastError,omitempty"`
	CreatedAt           time.Time        `json:"createdAt"`
	UpdatedAt           time.Time        `json:"updatedAt"`
}

type Options struct {
	// MaxJobs counts historical IDs, including released terminal tombstones.
	// Zero uses the pilot limit of 10,000 jobs. ReleaseSpool does not free slots.
	MaxJobs        int
	MaxSpoolBytes  int64
	MaxSourceBytes int64
	MaxResultBytes int64
	LeaseDuration  time.Duration
	RetryBase      time.Duration
	RetryMax       time.Duration
	MaxAttempts    int
	// Now is useful for deterministic scheduling and tests. It must be safe for
	// concurrent calls and return wall-clock time, which is persisted in UTC.
	Now func() time.Time
}

type ClaimRequest struct {
	WorkerID string `json:"workerId"`
}
type LeaseRequest struct {
	WorkerID string `json:"workerId"`
	Token    string `json:"token"`
}
type FailRequest struct {
	WorkerID string `json:"workerId"`
	Token    string `json:"token"`
	Error    string `json:"error"`
}
type ResultInput struct {
	SHA1     string `json:"sha1"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Filename string `json:"filename"`
}
