package coordinator

import (
	"context"

	"github.com/colthreepv/immich-optimizer/internal/adapter/replacement"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

func inputForJob(job jobqueue.Job) replacement.Input {
	return replacement.Input{OwnerID: job.OwnerID, OriginalAssetID: job.OriginalAssetID, SourceSHA1: job.SourceSHA1, SourceSHA256: job.SourceSHA256, SourceSize: job.Size, InputName: job.Filename, OutputName: job.OutputFilename, Preset: job.Preset, OutputPath: job.ResultPath, OutputSHA1: job.OutputSHA1, OutputSHA256: job.OutputSHA256, OutputSize: job.OutputSize, OutputAssetID: job.ResultAssetID}
}

// CollectOne releases queue-owned bytes only after checking the surviving
// Immich asset. Job hashes and deduplication records remain durable.
func (p *Publisher) CollectOne(ctx context.Context) error {
	jobs := p.Queue.List()
	for offset := 0; offset < len(jobs); offset++ {
		index := (p.gcCursor + offset) % len(jobs)
		job := jobs[index]
		if job.SourceSpoolPath == "" && job.ResultPath == "" {
			continue
		}
		if job.Status != jobqueue.StatusComplete && job.Status != jobqueue.StatusSkipped && job.Status != jobqueue.StatusFailed {
			continue
		}
		p.gcCursor = (index + 1) % len(jobs)
		var err error
		if job.Status == jobqueue.StatusComplete {
			err = p.Client.CanReleaseOutput(ctx, inputForJob(job))
		} else {
			err = p.Client.CanReleaseOriginal(ctx, job.OwnerID, job.OriginalAssetID, job.SourceSHA1)
		}
		if err != nil {
			return err
		}
		_, err = p.Queue.ReleaseSpool(job.ID)
		return err
	}
	return nil
}
