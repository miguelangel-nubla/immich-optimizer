// Package convert is the source-agnostic conversion engine. Every source
// (watcher, proxy, future apps) hands files to Engine.Convert; the engine
// selects the task, bounds global concurrency, decides between the processed
// output and the original, and owns the undone folder for failures.
package convert

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type Config struct {
	Tasks []entity.Task
	// MaxConcurrent is the global limit of concurrent conversions, shared by
	// all sources.
	MaxConcurrent int
	// TaskTimeout bounds the processing of one file; 0 means no limit.
	TaskTimeout time.Duration
	// SaveUnmatched saves files with no matching task to undone.
	SaveUnmatched bool
	// StripMotionPhoto, when set, implements "keep the photo, drop the
	// video" for motion photos: it writes the still of src to dst and
	// returns the size of the removed video, or ok=false (writing nothing)
	// when src is not a motion photo. The tasks then only see the still.
	StripMotionPhoto func(src, dst string) (videoBytes int64, ok bool, err error)
}

type Engine struct {
	cfg    Config
	proc   port.MediaProcessor
	undone port.UndoneStore
	log    *logger.Logger
	sem    chan struct{}
	now    func() time.Time
}

var _ port.Converter = (*Engine)(nil)

func New(cfg Config, proc port.MediaProcessor, undone port.UndoneStore, log *logger.Logger) *Engine {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	return &Engine{
		cfg:    cfg,
		proc:   proc,
		undone: undone,
		log:    log,
		sem:    make(chan struct{}, cfg.MaxConcurrent),
		now:    time.Now,
	}
}

// HasTask reports whether some task handles filename's extension.
func (e *Engine) HasTask(filename string) bool {
	ext := utils.NormalizeExtension(filepath.Ext(filename))
	for _, t := range e.cfg.Tasks {
		for _, te := range t.Extensions {
			if utils.NormalizeExtension(te) == ext {
				return true
			}
		}
	}
	return false
}

func (e *Engine) Convert(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
	if in.Filename == "" {
		in.Filename = filepath.Base(in.Path)
	}

	if !e.HasTask(in.Filename) {
		f := entity.Failure{
			Stage: entity.StageNoTask,
			Err:   fmt.Errorf("%w: %q", entity.ErrNoTask, filepath.Ext(in.Filename)),
			At:    e.now(),
		}
		return nil, e.fail(in, f, e.cfg.SaveUnmatched)
	}

	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, canceled(ctx)
	}
	pctx, cancel := ctx, context.CancelFunc(func() {})
	if e.cfg.TaskTimeout > 0 {
		pctx, cancel = context.WithTimeoutCause(ctx, e.cfg.TaskTimeout, entity.ErrTaskTimeout)
	}
	still := e.stripMotionPhoto(in)
	procPath := in.Path
	if still != nil {
		procPath = still.path
	}
	res, procErr := e.proc.Process(pctx, procPath, e.cfg.Tasks)
	timedOut := errors.Is(context.Cause(pctx), entity.ErrTaskTimeout)
	cancel()
	<-e.sem
	if still != nil && (procErr != nil || res == nil) {
		still.remove()
	}

	if procErr != nil {
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		f := entity.Failure{Stage: entity.StageConvert, Err: procErr, At: e.now()}
		var pe *entity.ProcessError
		if errors.As(procErr, &pe) {
			f.Attempts = pe.Attempts
		}
		if timedOut {
			f.Err = fmt.Errorf("%w (%s): %w", entity.ErrTaskTimeout, e.cfg.TaskTimeout, procErr)
			if n := len(f.Attempts); n > 0 {
				f.Attempts[n-1].TimedOut = true
			}
		}
		return nil, e.fail(in, f, true)
	}
	if res == nil {
		return nil, e.fail(in, entity.Failure{Stage: entity.StageConvert, Err: errors.New("processor returned no result"), At: e.now()}, true)
	}

	cleanup := res.Cleanup
	var once sync.Once
	release := func() {
		once.Do(func() {
			if cleanup != nil {
				cleanup()
			}
			if still != nil {
				still.remove()
			}
		})
	}

	var emptyErr error
	switch {
	case res.OriginalSize == 0:
		emptyErr = entity.ErrEmptyInput
	case res.ProcessedFilePath == "" || res.ProcessedSize == 0:
		emptyErr = entity.ErrEmptyOutput
	}
	if emptyErr != nil {
		release()
		return nil, e.fail(in, entity.Failure{Stage: entity.StageConvert, Err: emptyErr, At: e.now()}, true)
	}

	// With a motion photo the tasks saw only the still; report sizes against
	// the real input.
	originalSize := res.OriginalSize
	var videoBytes int64
	if still != nil {
		originalSize = still.originalSize
		videoBytes = still.videoBytes
	}

	inExt := filepath.Ext(in.Filename)
	if res.ProcessedSize < res.OriginalSize {
		outExt := filepath.Ext(res.ProcessedFilename)
		if outExt == "" {
			outExt = filepath.Ext(res.ProcessedFilePath)
		}
		if outExt == "" {
			outExt = inExt
		}
		r := &entity.ConvertResult{
			Outcome:      entity.OutcomeOptimized,
			DeliverPath:  res.ProcessedFilePath,
			DeliverName:  utils.TrimSuffixCaseInsensitive(in.Filename, inExt) + outExt,
			OriginalSize: originalSize,
			OutputSize:   res.ProcessedSize,
			Release:      release,

			MotionVideoBytes: videoBytes,
		}
		e.infof("Optimized %s (%s): %s -> %s", in.Filename, in.Source,
			utils.HumanReadableSize(r.OriginalSize), utils.HumanReadableSize(r.OutputSize))
		return r, nil
	}

	// Conversion gave no gain on a motion photo: deliver the still, which is
	// already smaller than the input.
	if still != nil {
		if cleanup != nil {
			cleanup()
		}
		e.infof("No size reduction for the still of %s (%s), delivering it without the motion video (%s -> %s)",
			in.Filename, in.Source, utils.HumanReadableSize(originalSize), utils.HumanReadableSize(res.OriginalSize))
		return &entity.ConvertResult{
			Outcome:      entity.OutcomeOptimized,
			DeliverPath:  still.path,
			DeliverName:  in.Filename,
			OriginalSize: originalSize,
			OutputSize:   res.OriginalSize,
			Release:      still.remove,

			MotionVideoBytes: videoBytes,
		}, nil
	}

	// Conversion succeeded but gave no gain: deliver the original. The
	// processed output is not needed any more.
	release()
	e.infof("No size reduction for %s (%s), delivering original (%s)", in.Filename, in.Source,
		utils.HumanReadableSize(res.OriginalSize))
	return &entity.ConvertResult{
		Outcome:      entity.OutcomeUnchanged,
		DeliverPath:  in.Path,
		DeliverName:  in.Filename,
		OriginalSize: res.OriginalSize,
		OutputSize:   res.OriginalSize,
		Release:      func() {},
	}, nil
}

// motionStill is the temporary still extracted from a motion photo.
type motionStill struct {
	path         string
	originalSize int64
	videoBytes   int64
	once         sync.Once
}

func (m *motionStill) remove() {
	m.once.Do(func() { os.RemoveAll(filepath.Dir(m.path)) })
}

// stripMotionPhoto returns the still of a JPEG motion photo, or nil when the
// input is not one. A stripping error is not fatal: the file is converted as
// it is, which is what happened before this policy existed.
func (e *Engine) stripMotionPhoto(in entity.ConvertInput) *motionStill {
	if e.cfg.StripMotionPhoto == nil {
		return nil
	}
	switch utils.NormalizeExtension(filepath.Ext(in.Filename)) {
	case "jpg", "jpeg":
	default:
		return nil
	}
	st, err := os.Stat(in.Path)
	if err != nil {
		return nil
	}
	dir, err := os.MkdirTemp("", "io-motion-*")
	if err != nil {
		e.warnf("Motion photo check skipped for %s: %v", in.Filename, err)
		return nil
	}
	// Keep the input's extension: it selects the task.
	path := filepath.Join(dir, "still"+filepath.Ext(in.Filename))
	videoBytes, ok, err := e.cfg.StripMotionPhoto(in.Path, path)
	if err != nil || !ok {
		os.RemoveAll(dir)
		if err != nil {
			e.warnf("Motion photo check failed for %s, converting it as it is: %v", in.Filename, err)
		}
		return nil
	}
	e.infof("Motion photo %s (%s): keeping the photo, dropping the embedded video (%s)",
		in.Filename, in.Source, utils.HumanReadableSize(videoBytes))
	return &motionStill{path: path, originalSize: st.Size(), videoBytes: videoBytes}
}

// SaveUndone records a failure that happened after conversion.
func (e *Engine) SaveUndone(in entity.ConvertInput, f entity.Failure) (*entity.UndoneRecord, error) {
	if f.At.IsZero() {
		f.At = e.now()
	}
	if in.Filename == "" {
		in.Filename = filepath.Base(in.Path)
	}
	rec, err := e.save(in, f)
	e.logFailure(in, f, rec, err, true)
	return rec, err
}

func (e *Engine) save(in entity.ConvertInput, f entity.Failure) (*entity.UndoneRecord, error) {
	if e.undone == nil {
		return nil, errors.New("no undone store configured")
	}
	return e.undone.Save(in, f)
}

func (e *Engine) fail(in entity.ConvertInput, f entity.Failure, save bool) error {
	ce := &entity.ConversionError{Filename: in.Filename, Failure: f}
	if save {
		ce.Undone, ce.UndoneErr = e.save(in, f)
	}
	e.logFailure(in, f, ce.Undone, ce.UndoneErr, save)
	return ce
}

func (e *Engine) logFailure(in entity.ConvertInput, f entity.Failure, rec *entity.UndoneRecord, saveErr error, saved bool) {
	if e.log == nil {
		return
	}
	reason := "no error"
	if f.Err != nil {
		reason = f.Err.Error()
		var pe *entity.ProcessError
		if errors.As(f.Err, &pe) {
			// Keep the log line short; full output is in the note and in debug.
			reason = pe.Err.Error()
			e.log.Debugf("Conversion details for %s: %v", in.Filename, f.Err)
		}
	}
	stage := "Conversion"
	switch f.Stage {
	case entity.StageNoTask:
		stage = "No task"
	case entity.StageUpload:
		stage = "Upload"
	}
	switch {
	case !saved:
		e.log.Printf("%s failed for %s (%s): %s; not saved to undone", stage, in.Filename, in.Source, reason)
	case saveErr != nil:
		e.log.Errorf("%s failed for %s (%s): %s; could NOT save to undone: %v", stage, in.Filename, in.Source, reason, saveErr)
	default:
		e.log.Printf("%s failed for %s (%s): %s; saved to %s (seen %dx)", stage, in.Filename, in.Source, reason, rec.Dir, rec.Occurrences)
	}
}

func (e *Engine) warnf(format string, args ...any) {
	if e.log != nil {
		e.log.Warnf(format, args...)
	}
}

func (e *Engine) infof(format string, args ...any) {
	if e.log != nil {
		e.log.Printf(format, args...)
	}
}

func canceled(ctx context.Context) error {
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return fmt.Errorf("%w: %w", entity.ErrCanceled, cause)
}
