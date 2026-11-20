package port

import (
	"context"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

type MediaProcessor interface {
	Process(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error)
	// Verify runs task.Verify on an original and the output a task made
	// from it. A nil error means the command exited with status 0.
	Verify(ctx context.Context, task entity.Task, originalPath, outputPath string) error
}
