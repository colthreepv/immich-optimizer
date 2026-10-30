package coordinator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/colthreepv/immich-optimizer/internal/adapter/replacement"
	"github.com/colthreepv/immich-optimizer/internal/adapter/state"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

// Publisher is the sole queue publisher and ledger writer. Workers cannot publish.
type Publisher struct {
	Queue             *jobqueue.Store
	Ledger            *state.Store
	Client            *replacement.Client
	RetireOriginal    bool
	Logger            *log.Logger
	ReconcileOriginal func(context.Context, jobqueue.Job) (string, error)
	VerifyAVIF        func(context.Context, string, string) error
	reconcileCursor   int
	gcCursor          int
}

func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	nextReconcile := time.Time{}
	nextGC := time.Time{}
	for {
		if ctx.Err() != nil {
			return
		}
		if p.ReconcileOriginal != nil && !time.Now().Before(nextReconcile) {
			p.reconcileOne(ctx)
			nextReconcile = time.Now().Add(30 * time.Second)
		}
		if !time.Now().Before(nextGC) {
			if err := p.CollectOne(ctx); err != nil && p.Logger != nil {
				p.Logger.Printf("Queue spool cleanup: %v", err)
			}
			nextGC = time.Now().Add(10 * time.Second)
		}
		job, err := p.Queue.NextPublication()
		if err == nil {
			if err = p.Process(ctx, job); err != nil && ctx.Err() == nil {
				if errors.Is(err, replacement.ErrBlocked) {
					_, _ = p.Queue.HoldPublication(job.ID, err.Error())
				} else {
					_, _ = p.Queue.RetryPublication(job.ID, err.Error())
				}
				if p.Logger != nil {
					p.Logger.Printf("Publication job %s: %v", job.ID, err)
				}
			}
		} else if !errors.Is(err, jobqueue.ErrNoJobs) && p.Logger != nil {
			p.Logger.Printf("Publication queue: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Publisher) reconcileOne(ctx context.Context) {
	jobs := p.Queue.List()
	for offset := 0; offset < len(jobs); offset++ {
		index := (p.reconcileCursor + offset) % len(jobs)
		job := jobs[index]
		if job.Status != jobqueue.StatusPublishingOriginal || job.OriginalAssetID != "" {
			continue
		}
		id, err := p.ReconcileOriginal(ctx, job)
		if err == nil {
			_, err = p.Queue.MarkOriginalUploaded(job.ID, id)
		}
		if err != nil && p.Logger != nil {
			p.Logger.Printf("Original publication job %s: %v", job.ID, err)
		}
		p.reconcileCursor = (index + 1) % len(jobs)
		return
	}
}

func (p *Publisher) Process(ctx context.Context, job jobqueue.Job) error {
	if job.PublicationStage == jobqueue.StageOriginalTrashed {
		_, err := p.Queue.AdvancePublication(job.ID, jobqueue.StageOriginalTrashed, jobqueue.StageComplete, job.ResultAssetID)
		return err
	}
	if !p.RetireOriginal && stageIndex(job.PublicationStage) < stageIndex(jobqueue.StageMetadataMigrated) {
		return fmt.Errorf("%w: automatic replacement disabled; result retained before publication", replacement.ErrBlocked)
	}
	if err := auditQueuedParts(job); err != nil {
		return err
	}
	in := inputForJob(job)
	in.VerifyAVIF = func(ctx context.Context, _ string, path string) error {
		validate := p.VerifyAVIF
		if validate == nil {
			validate = ValidateAVIF
		}
		return validate(ctx, job.SourceSpoolPath, path)
	}
	var result replacement.Result
	if stageIndex(job.PublicationStage) < stageIndex(jobqueue.StageMetadataMigrated) {
		var err error
		result, err = p.Client.Publish(ctx, in, func(_ context.Context, next replacement.Stage, r replacement.Result) error {
			target := jobqueue.PublicationStage(next)
			if stageIndex(target) <= stageIndex(job.PublicationStage) {
				if job.ResultAssetID != r.OutputAssetID {
					return fmt.Errorf("output asset checkpoint conflict")
				}
				return nil
			}
			updated, err := p.Queue.AdvancePublication(job.ID, job.PublicationStage, target, r.OutputAssetID)
			if err == nil {
				job = updated
			}
			return err
		})
		if err != nil {
			return err
		}
	} else {
		sourceSize, outputSize := job.Size, job.OutputSize
		result = replacement.Result{OwnerID: job.OwnerID, OriginalAssetID: job.OriginalAssetID, OutputAssetID: job.ResultAssetID, SourceSHA1: job.SourceSHA1, OutputSHA1: job.OutputSHA1, OutputSHA256: job.OutputSHA256, SourceSize: &sourceSize, OutputSize: &outputSize}
	}
	if previous, ok := p.Ledger.FindSource(job.OwnerID, job.SourceSHA1); ok && previous.AssetID != job.OriginalAssetID && previous.AssetID != result.OutputAssetID {
		return fmt.Errorf("%w: conflicting source alias", replacement.ErrBlocked)
	}
	mapping := state.Record{OwnerID: job.OwnerID, AssetID: result.OutputAssetID, SourceSHA1: job.SourceSHA1, OutputSHA1: job.OutputSHA1, SourceFilename: job.Filename, OutputFilename: job.OutputFilename, Preset: job.Preset, State: "verified", SourceSize: job.Size, OutputSize: job.OutputSize, UpdatedAt: time.Now().UTC()}
	if err := p.Ledger.Put(mapping); err != nil {
		return err
	}
	if !job.SyncRepublished {
		if err := p.Client.Republish(ctx, job.OwnerID, result.OutputAssetID); err != nil {
			return err
		}
		updated, err := p.Queue.MarkSyncRepublished(job.ID)
		if err != nil {
			return err
		}
		job = updated
	}
	if !p.RetireOriginal {
		return fmt.Errorf("%w: replacement verified; original retirement disabled", replacement.ErrBlocked)
	}
	in.OutputAssetID = result.OutputAssetID
	if err := p.Client.Retire(ctx, in, result, true); err != nil {
		return err
	}
	updated, err := p.Queue.AdvancePublication(job.ID, job.PublicationStage, jobqueue.StageOriginalTrashed, result.OutputAssetID)
	if err != nil {
		return err
	}
	_, err = p.Queue.AdvancePublication(job.ID, updated.PublicationStage, jobqueue.StageComplete, result.OutputAssetID)
	return err
}

func stageIndex(stage jobqueue.PublicationStage) int {
	for i, value := range []jobqueue.PublicationStage{jobqueue.StagePending, jobqueue.StageUploaded, jobqueue.StageVerified, jobqueue.StageMetadataMigrated, jobqueue.StageOriginalTrashed, jobqueue.StageComplete} {
		if value == stage {
			return i
		}
	}
	return -1
}
