// Package pipeline is the watcher source: it hands files from the watch
// directory to the conversion engine, uploads the result to Immich and
// removes the source once it is either uploaded or saved to undone.
package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type Service struct {
	audit    *audit.Log
	watcher  port.FileWatcher
	conv     port.Converter
	uploader port.AssetUploader
	fs       port.FileSystem
	logger   *logger.Logger
	watchDir string

	semaphore chan struct{}
	// handlers bounds the events being handled or waiting for a place in
	// semaphore. Beyond it the events wait in the watcher: a scan of a large
	// backlog does not start one handler per file at once.
	handlers   chan struct{}
	processing sync.Map
	stopChan   chan struct{}
	stopOnce   sync.Once
	ctx        context.Context
	cancel     context.CancelCauseFunc
	wg         sync.WaitGroup
}

func NewService(
	watcher port.FileWatcher,
	conv port.Converter,
	uploader port.AssetUploader,
	fs port.FileSystem,
	log *logger.Logger,
	maxConcurrent int,
	watchDir string,
	auditLog *audit.Log,
) *Service {
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Service{
		watcher:   watcher,
		conv:      conv,
		uploader:  uploader,
		fs:        fs,
		logger:    log,
		watchDir:  watchDir,
		audit:     auditLog,
		semaphore: make(chan struct{}, maxConcurrent),
		handlers:  make(chan struct{}, maxConcurrent*4+64),
		stopChan:  make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (s *Service) Start() {
	s.wg.Add(1)
	go s.consumeEvents()
}

// Drain stops accepting events and waits for in-flight files until ctx is done.
func (s *Service) Drain(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stopChan) })
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel aborts in-flight conversions and uploads. Affected files stay in the
// watch directory and are picked up again at the next start.
func (s *Service) Cancel() {
	s.cancel(entity.ErrShuttingDown)
}

// Stop drains without a deadline. Kept for tests and simple callers.
func (s *Service) Stop() {
	_ = s.Drain(context.Background())
}

func (s *Service) consumeEvents() {
	defer s.wg.Done()
	events := s.watcher.Events()

	for {
		select {
		case <-s.stopChan:
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			select {
			case s.handlers <- struct{}{}:
			case <-s.stopChan:
				return
			}
			s.wg.Add(1)
			go func(e entity.FileEvent) {
				defer s.wg.Done()
				defer func() { <-s.handlers }()
				s.handleEvent(s.ctx, e)
			}(event)
		}
	}
}

// inFlight is the state of a path that is being handled.
type inFlight struct {
	// again is set when another event for the path arrived meanwhile.
	again atomic.Bool
}

func (s *Service) handleEvent(ctx context.Context, event entity.FileEvent) {
	path := event.Path
	// Only one handler per path. A second event is not lost: it is noted, and
	// matters when the file turns out to have changed while it was handled.
	st := &inFlight{}
	if cur, loaded := s.processing.LoadOrStore(path, st); loaded {
		cur.(*inFlight).again.Store(true)
		return
	}
	defer s.processing.CompareAndDelete(path, st)

	for {
		st.again.Store(false)
		if !s.processFile(ctx, path) {
			return
		}
		// The file was written to while it was handled, so it was kept. If
		// an event arrived meanwhile it may have been the one that announced
		// the new content: handle the file again. Otherwise the write is
		// still going on and its own event will come.
		if st.again.Load() {
			continue
		}
		s.processing.CompareAndDelete(path, st)
		if !st.again.Load() {
			return
		}
		// An event slipped in just before the path was released.
		if _, loaded := s.processing.LoadOrStore(path, st); loaded {
			return
		}
	}
}

// processFile converts and uploads one file and removes it from the watch
// directory. It reports true when the file was kept because it changed while
// it was handled.
func (s *Service) processFile(ctx context.Context, path string) (changed bool) {
	if utils.IsTempFile(path) || utils.IsUndoneArtifact(path) || utils.IsIgnoredFile(path) {
		return false
	}
	// Also skip everything inside hidden folders (.stfolder, .thumbnails...).
	for _, dir := range strings.Split(filepath.ToSlash(s.relDir(path)), "/") {
		if strings.HasPrefix(dir, ".") {
			return false
		}
	}

	// Files still queued when shutdown starts are left in the watch directory
	// for the next start instead of being started during the grace period.
	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	case <-ctx.Done():
		return false
	case <-s.stopChan:
		return false
	}
	select {
	case <-s.stopChan:
		return false
	default:
	}

	info, err := os.Stat(path)
	if err != nil {
		// A second announcement of a file that is already done and removed
		// is normal.
		if errors.Is(err, os.ErrNotExist) {
			s.logger.Debugf("Nothing to do for %s: it is gone", path)
		} else {
			s.logger.Printf("Error getting file info for %s: %v", path, err)
		}
		return false
	}
	if info.IsDir() {
		return false
	}

	s.logger.Printf("Processing file: %s", path)

	in := entity.ConvertInput{
		Path:        path,
		Filename:    filepath.Base(path),
		Source:      entity.SourceWatcher,
		OriginalDir: s.relDir(path),
		ModTime:     info.ModTime(),
	}

	res, err := s.conv.Convert(ctx, in)
	if err != nil {
		var ce *entity.ConversionError
		switch {
		case errors.Is(err, entity.ErrCanceled) || ctx.Err() != nil:
			s.logger.Printf("Processing of %s canceled; it stays in the watch directory: %v", path, err)
		case errors.As(err, &ce) && ce.Undone != nil:
			return s.removeSource(path, info, "after saving it to undone")
		default:
			// Not saved anywhere: never delete the only copy.
			s.logger.Errorf("Keeping %s in the watch directory: %v", path, err)
		}
		return false
	}
	defer res.Release()

	// A file written to while it was converted: what was converted (and the
	// checksum in its marker) is not what the file holds now. Nothing is
	// uploaded; the file is handled again.
	if !unchanged(path, info) {
		s.logger.Warnf("Not uploading %s: it was written to while it was being converted; it is handled again", path)
		return true
	}

	upload := entity.Upload{
		Path:     res.DeliverPath,
		Filename: res.DeliverName,
		// The asset's dates come from the original file, not from the
		// freshly written conversion output.
		ModTime: info.ModTime(),
		Marker:  res.Marker,
	}
	up, err := s.uploader.UploadAsset(ctx, upload)
	if err != nil {
		if ctx.Err() != nil {
			s.logger.Printf("Upload of %s canceled; it stays in the watch directory: %v", path, err)
			return false
		}
		f := entity.Failure{Stage: entity.StageUpload, Err: err}
		rec, saveErr := s.conv.SaveUndone(in, f)
		entry := audit.Entry{Source: string(entity.SourceWatcher), Event: "upload_failed", File: in.Filename, Detail: err.Error()}
		if rec != nil {
			entry.Undone, entry.OriginalSHA1 = rec.Dir, rec.SHA1
		}
		s.record(entry)
		if saveErr != nil {
			s.logger.Errorf("Keeping %s in the watch directory: %v", path, saveErr)
			return false
		}
		return s.removeSource(path, info, "after saving it to undone")
	}

	s.record(audit.Delivered(entity.SourceWatcher, in.Filename, res, up))

	if res.Outcome == entity.OutcomeOptimized {
		s.logger.Printf("Optimized file uploaded: %s -> %s",
			utils.HumanReadableSize(res.OriginalSize), utils.HumanReadableSize(res.OutputSize))
	} else {
		s.logger.Printf("Original file uploaded (no optimization achieved)")
	}
	return s.removeSource(path, info, "after upload")
}

// record writes an audit entry; the watcher keeps working if it cannot.
func (s *Service) record(e audit.Entry) {
	if err := s.audit.Write(e); err != nil {
		s.logger.Warnf("Could not write the audit log: %v", err)
	}
}

func (s *Service) relDir(path string) string {
	if s.watchDir == "" {
		return ""
	}
	rel, err := filepath.Rel(s.watchDir, filepath.Dir(path))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return ""
	}
	return rel
}

// unchanged reports whether path is still the file described by handled: the
// same file, size and modification time. A file that cannot be read now
// counts as unchanged; whoever acts on it next finds out.
func unchanged(path string, handled os.FileInfo) bool {
	now, err := os.Stat(path)
	if err != nil {
		return true
	}
	return os.SameFile(handled, now) && now.Size() == handled.Size() && now.ModTime().Equal(handled.ModTime())
}

// removeSource removes a handled file from the watch directory, unless it is
// no longer the file that was handled: one that was written to or replaced
// meanwhile has content that was never converted or uploaded, so it is kept
// and true is returned.
func (s *Service) removeSource(path string, handled os.FileInfo, why string) (changed bool) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false
	}
	if !unchanged(path, handled) {
		s.logger.Warnf("Keeping %s in the watch directory: it was written to while it was being processed", path)
		return true
	}
	if err := s.fs.RemoveFile(path); err != nil {
		s.logger.Printf("Error removing file %s %s: %v", path, why, err)
	}
	return false
}
