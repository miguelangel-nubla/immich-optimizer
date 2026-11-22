// Package convert is the source-agnostic conversion engine. Every source
// (watcher, proxy, future apps) hands files to Engine.Convert; the engine
// selects the task, bounds global concurrency, decides between the processed
// output and the original, and owns the undone folder for failures.
package convert

import (
	"context"
	"crypto/sha1" // #nosec G505 -- content identity matching Immich, not security
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
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
	// Slots, when set, is that limit shared with other engines (one per
	// profile): it replaces MaxConcurrent.
	Slots chan struct{}
	// TaskTimeout bounds the processing of one file; 0 means no limit.
	TaskTimeout time.Duration
	// SaveUnmatched saves files with no matching task to undone.
	SaveUnmatched bool
	// Version is the optimizer version recorded in provenance markers, and
	// ConfigSHA256 the fingerprint of the tasks file's folder.
	Version      string
	ConfigSHA256 string
	// Audit, when set, records every input the engine refused or failed to
	// convert (library mode records its own, richer entries).
	Audit *audit.Log
	// Checker refuses a result that is tiny next to its original, in every
	// mode, and validates converted files for library mode.
	Checker port.OutputChecker
}

type Engine struct {
	cfg    Config
	proc   port.MediaProcessor
	undone port.UndoneStore
	log    *logger.Logger
	sem    chan struct{}
	now    func() time.Time
}

var _ port.CheckingConverter = (*Engine)(nil)

func New(cfg Config, proc port.MediaProcessor, undone port.UndoneStore, log *logger.Logger) *Engine {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.Slots == nil {
		cfg.Slots = make(chan struct{}, cfg.MaxConcurrent)
	}
	return &Engine{
		cfg:    cfg,
		proc:   proc,
		undone: undone,
		log:    log,
		sem:    cfg.Slots,
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

	// The audit log is mandatory: nothing is converted or delivered without
	// a place to record it.
	if err := e.cfg.Audit.Check(); err != nil {
		if e.log != nil {
			e.log.Errorf("Refusing %s (%s): the audit log cannot be written: %v", in.Filename, in.Source, err)
		}
		return nil, fmt.Errorf("%w: %w", entity.ErrAuditUnavailable, err)
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
	pctx = entity.WithTaskInfo(pctx, entity.TaskInfo{OriginalName: in.Filename, Source: string(in.Source)})
	res, procErr := e.proc.Process(pctx, in.Path, e.cfg.Tasks)
	timedOut := errors.Is(context.Cause(pctx), entity.ErrTaskTimeout)
	cancel()
	<-e.sem

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

	inExt := filepath.Ext(in.Filename)
	if res.ProcessedSize < res.OriginalSize && e.cfg.Checker != nil {
		// A valid, much smaller file is exactly what a wrong conversion
		// looks like too: the thumbnail of a photo instead of the photo.
		if err := e.cfg.Checker.CheckSize(ctx, in.Path, res.ProcessedFilePath); err != nil {
			release()
			return nil, e.fail(in, entity.Failure{Stage: entity.StageConvert, Err: err, At: e.now()}, true)
		}
	}
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
			OriginalSize: res.OriginalSize,
			OutputSize:   res.ProcessedSize,
			Release:      release,
		}
		sum, err := originalSHA1(in)
		if err != nil {
			release()
			return nil, e.fail(in, entity.Failure{Stage: entity.StageConvert, Err: fmt.Errorf("hashing the original: %w", err), At: e.now()}, true)
		}
		outSum, err := originalSHA1(entity.ConvertInput{Path: res.ProcessedFilePath})
		if err != nil {
			release()
			return nil, e.fail(in, entity.Failure{Stage: entity.StageConvert, Err: fmt.Errorf("hashing the converted file: %w", err), At: e.now()}, true)
		}
		r.Marker = &entity.Marker{
			V:            entity.MarkerVersion,
			SHA1:         sum,
			Size:         res.OriginalSize,
			OriginalName: in.Filename,
			Task:         res.Task,
			TaskCommand:  res.TaskCommand,
			ConfigSHA256: e.cfg.ConfigSHA256,
			MotionPhotos: res.MotionPhotos,
			Source:       string(in.Source),
			Optimizer:    e.cfg.Version,
			At:           e.now(),
			OutputSize:   res.ProcessedSize,
			OutputName:   r.DeliverName,
			OutputSHA1:   outSum,
		}
		note := ""
		if res.MotionPhotos != "" {
			note = ", motion photo: " + res.MotionPhotos
		}
		e.infof("Optimized %s (%s%s): %s -> %s", in.Filename, in.Source, note,
			utils.HumanReadableSize(r.OriginalSize), utils.HumanReadableSize(r.OutputSize))
		return r, nil
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

// originalSHA1 returns the lowercase hex SHA-1 of the input, the checksum
// Immich would store for it. Sources that already hashed the file pass it in.
// originalSHA1 returns in.SHA1 when it is a checksum, else hashes in.Path.
func originalSHA1(in entity.ConvertInput) (string, error) {
	if len(in.SHA1) == sha1.Size*2 {
		if _, err := hex.DecodeString(in.SHA1); err == nil {
			return strings.ToLower(in.SHA1), nil
		}
	}
	f, err := os.Open(in.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New() // #nosec G401 -- content identity matching Immich, not security
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Converts reports whether filename's first matching task converts the file
// (rather than passing it through).
func (e *Engine) Converts(filename string) (bool, string) {
	ext := utils.NormalizeExtension(filepath.Ext(filename))
	for _, t := range e.cfg.Tasks {
		for _, te := range t.Extensions {
			if utils.NormalizeExtension(te) != ext {
				continue
			}
			if strings.TrimSpace(t.Command) == "" {
				return false, "its task (" + t.Name + ") passes files through unchanged"
			}
			return true, ""
		}
	}
	return false, "no task handles this file type"
}

// CheckOutput reports an error when the converted file does not look like a
// valid photo or video.
func (e *Engine) CheckOutput(ctx context.Context, in entity.ConvertInput, res *entity.ConvertResult) error {
	if res == nil || res.Outcome != entity.OutcomeOptimized {
		return errors.New("nothing was converted")
	}
	if e.cfg.Checker == nil {
		return errors.New("no output checker configured")
	}
	return e.cfg.Checker.Check(ctx, in.Path, res.DeliverPath)
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
	save = save && !in.SkipUndone
	if save {
		ce.Undone, ce.UndoneErr = e.save(in, f)
	}
	e.logFailure(in, f, ce.Undone, ce.UndoneErr, save)
	e.auditFailure(in, f, ce.Undone)
	return ce
}

// auditFailure records an input that was not delivered.
func (e *Engine) auditFailure(in entity.ConvertInput, f entity.Failure, rec *entity.UndoneRecord) {
	if e.cfg.Audit == nil || in.Source == entity.SourceLibrary {
		return
	}
	entry := audit.Entry{Source: string(in.Source), Event: "failed", File: in.Filename, OriginalSHA1: in.SHA1}
	if f.Stage == entity.StageNoTask {
		entry.Event = "refused"
	}
	if f.Err != nil {
		entry.Detail = f.Err.Error()
		var pe *entity.ProcessError
		if errors.As(f.Err, &pe) {
			entry.Detail = pe.Err.Error()
		}
	}
	if rec != nil {
		entry.Undone = rec.Dir
		if entry.OriginalSHA1 == "" {
			entry.OriginalSHA1 = rec.SHA1
		}
	}
	if err := e.cfg.Audit.Write(entry); err != nil {
		e.warnf("Could not write the audit log: %v", err)
	}
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
