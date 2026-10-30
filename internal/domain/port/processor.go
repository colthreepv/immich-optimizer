package port

import (
	"context"

	"github.com/colthreepv/immich-optimizer/internal/domain/entity"
)

type MediaProcessor interface {
	Process(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error)
}
