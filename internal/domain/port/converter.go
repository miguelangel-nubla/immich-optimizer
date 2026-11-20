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

// VerifyingConverter is a Converter that can also prove its results. Library
// mode needs it: it only replaces an asset whose conversion was verified.
type VerifyingConverter interface {
	Converter
	// Verify runs the verify command of the task that produced res, proving
	// that res is a faithful conversion of in. It returns an error wrapping
	// entity.ErrNoVerify when that task has none.
	Verify(ctx context.Context, in entity.ConvertInput, res *entity.ConvertResult) error
	// Verifiable reports whether a file with this name would be converted by
	// a task that has a verify command. When it would not, reason says why.
	Verifiable(filename string) (ok bool, reason string)
}
