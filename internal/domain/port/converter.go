package port

import (
	"context"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// Converter is the source-agnostic conversion engine API. Every source
// (watcher, proxy, future apps) hands files to it and delivers the result.
type Converter interface {
	// Convert returns a deliverable result, or an error:
	//   - *entity.ConversionError: no task matched or conversion failed; the
	//     input has already been saved to undone according to policy.
	//   - an error wrapping entity.ErrCanceled: ctx was cancelled; nothing saved.
	Convert(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error)
	// SaveUndone records a failure that happened after conversion (e.g. the
	// watcher's upload stage).
	SaveUndone(in entity.ConvertInput, f entity.Failure) (*entity.UndoneRecord, error)
}
