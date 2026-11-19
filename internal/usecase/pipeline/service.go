// Package pipeline is the watcher source: it hands files from the watch
// directory to the conversion engine, uploads the result to Immich and
// removes the source once it is either uploaded or saved to undone.
package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type Service struct {
	watcher  port.FileWatcher
	conv     port.Converter
	uploader port.AssetUploader
	fs       port.FileSystem
	logger   *logger.Logger
	watchDir string

	semaphore  chan struct{}
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
		semaphore: make(chan struct{}, maxConcurrent),
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
			s.wg.Add(1)
			go func(e entity.FileEvent) {
				defer s.wg.Done()
				s.handleEvent(s.ctx, e)
			}(event)
		}
	}
}

func (s *Service) handleEvent(ctx context.Context, event entity.FileEvent) {
	path := event.Path
	// Deduplicate parallel events: abort if this file is already running
	if _, loaded := s.processing.LoadOrStore(path, true); loaded {
		return
	}
	defer s.processing.Delete(path)

	if utils.IsTempFile(path) || utils.IsUndoneArtifact(path) {
		return
	}

	// Files still queued when shutdown starts are left in the watch directory
	// for the next start instead of being started during the grace period.
	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	case <-ctx.Done():
		return
	case <-s.stopChan:
		return
	}
	select {
	case <-s.stopChan:
		return
	default:
	}

	info, err := os.Stat(path)
	if err != nil {
		s.logger.Printf("Error getting file info for %s: %v", path, err)
		return
	}
	if info.IsDir() {
		return
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
			s.removeSource(path, "after saving it to undone")
		default:
			// Not saved anywhere: never delete the only copy.
			s.logger.Errorf("Keeping %s in the watch directory: %v", path, err)
		}
		return
	}
	defer res.Release()

	if err := s.uploader.UploadAssetWithFilename(ctx, res.DeliverPath, res.DeliverName); err != nil {
		if ctx.Err() != nil {
			s.logger.Printf("Upload of %s canceled; it stays in the watch directory: %v", path, err)
			return
		}
		f := entity.Failure{Stage: entity.StageUpload, Err: err}
		if _, saveErr := s.conv.SaveUndone(in, f); saveErr != nil {
			s.logger.Errorf("Keeping %s in the watch directory: %v", path, saveErr)
			return
		}
		s.removeSource(path, "after saving it to undone")
		return
	}

	if res.Outcome == entity.OutcomeOptimized {
		s.logger.Printf("Optimized file uploaded: %s -> %s",
			utils.HumanReadableSize(res.OriginalSize), utils.HumanReadableSize(res.OutputSize))
	} else {
		s.logger.Printf("Original file uploaded (no optimization achieved)")
	}
	s.removeSource(path, "after upload")
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

func (s *Service) removeSource(path, why string) {
	if err := s.fs.RemoveFile(path); err != nil {
		s.logger.Printf("Error removing file %s %s: %v", path, why, err)
	}
}
