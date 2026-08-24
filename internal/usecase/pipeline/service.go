package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type Service struct {
	watcher   port.FileWatcher
	processor port.MediaProcessor
	uploader  port.AssetUploader
	fs        port.FileSystem
	logger    *logger.Logger
	tasks     []entity.Task

	semaphore  chan struct{}
	processing sync.Map
	stopChan   chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

func NewService(
	watcher port.FileWatcher,
	processor port.MediaProcessor,
	uploader port.AssetUploader,
	fs port.FileSystem,
	log *logger.Logger,
	tasks []entity.Task,
	maxConcurrent int,
) *Service {
	return &Service{
		watcher:   watcher,
		processor: processor,
		uploader:  uploader,
		fs:        fs,
		logger:    log,
		tasks:     tasks,
		semaphore: make(chan struct{}, maxConcurrent),
		stopChan:  make(chan struct{}),
	}
}

func (s *Service) Start() {
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Add(1)
	go s.consumeEvents()
}

func (s *Service) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	close(s.stopChan)
	s.wg.Wait()
}

func (s *Service) consumeEvents() {
	defer s.wg.Done()
	events := s.watcher.Events()

	for {
		select {
		case <-s.stopChan:
			return
		case event := <-events:
			s.wg.Add(1)
			go func(e entity.FileEvent) {
				defer s.wg.Done()
				s.handleEvent(s.ctx, e)
			}(event)
		}
	}
}

func (s *Service) handleEvent(ctx context.Context, event entity.FileEvent) {
	// Deduplicate parallel events: abort if this file is already running
	if _, loaded := s.processing.LoadOrStore(event.Path, true); loaded {
		return
	}
	defer s.processing.Delete(event.Path)

	s.semaphore <- struct{}{}
	defer func() { <-s.semaphore }()

	if !s.validateFile(event.Path) {
		return
	}

	if utils.IsTempFile(event.Path) {
		return
	}

	s.logger.Printf("Processing file: %s", event.Path)

	if !s.shouldOptimizeFile(event.Path) {
		s.handleProcessingError(ctx, event.Path, fmt.Errorf("no task found for file extension %s", filepath.Ext(event.Path)))
		return
	}

	result, err := s.processor.Process(ctx, event.Path, s.tasks)
	if err != nil {
		s.handleProcessingError(ctx, event.Path, err)
		return
	}
	if result != nil && result.Cleanup != nil {
		defer result.Cleanup()
	}

	if err := s.handleProcessingSuccess(ctx, event.Path, result); err != nil {
		s.handleUploadError(ctx, event.Path, err)
		return
	}

	if err := s.fs.RemoveFile(event.Path); err != nil {
		s.logger.Printf("Error removing file %s after upload: %v", event.Path, err)
	}
}

func (s *Service) validateFile(filePath string) bool {
	info, err := os.Stat(filePath)
	if err != nil {
		s.logger.Printf("Error getting file info for %s: %v", filePath, err)
		return false
	}
	return !info.IsDir()
}

func (s *Service) shouldOptimizeFile(filePath string) bool {
	extension := filepath.Ext(filePath)
	checkExt := utils.NormalizeExtension(extension)
	for _, task := range s.tasks {
		for _, ext := range task.Extensions {
			if utils.NormalizeExtension(ext) == checkExt {
				return true
			}
		}
	}
	s.logger.Printf("Skipping file %s (extension %s not configured for processing)", filePath, extension)
	return false
}

func (s *Service) handleFailure(ctx context.Context, filePath, stage string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil) {
		s.logger.Printf("Operation %s for file %s canceled due to context termination: %v", stage, filePath, err)
		return
	}
	s.logger.Printf("Error %s file %s: %v", stage, filePath, err)
	if moveErr := s.fs.MoveToUndone(filePath); moveErr != nil {
		s.logger.Printf("Error moving file %s to undone directory: %v", filePath, moveErr)
	} else {
		s.logger.Printf("Moved file %s to undone directory", filePath)
	}
}

func (s *Service) handleProcessingError(ctx context.Context, filePath string, err error) {
	s.handleFailure(ctx, filePath, "processing", err)
}

func (s *Service) handleUploadError(ctx context.Context, filePath string, err error) {
	s.handleFailure(ctx, filePath, "uploading", err)
}

func (s *Service) handleProcessingSuccess(ctx context.Context, originalFilePath string, result *entity.ProcessResult) error {
	if s.shouldUploadProcessedFile(result) {
		return s.uploadProcessedFile(ctx, originalFilePath, result)
	}
	return s.uploadOriginalFile(ctx, originalFilePath)
}

func (s *Service) shouldUploadProcessedFile(result *entity.ProcessResult) bool {
	if result == nil || result.ProcessedSize == 0 {
		return false
	}
	return result.ProcessedSize < result.OriginalSize
}

func (s *Service) uploadProcessedFile(ctx context.Context, originalFilePath string, result *entity.ProcessResult) error {
	if result.ProcessedFilePath == "" {
		s.logger.Printf("Error getting processed file path: no path available")
		return s.uploader.UploadAsset(ctx, originalFilePath)
	}

	processedFilename := result.ProcessedFilename
	if processedFilename == "" {
		processedFilename = filepath.Base(originalFilePath)
	}

	if err := s.uploader.UploadAssetWithFilename(ctx, result.ProcessedFilePath, processedFilename); err != nil {
		return err
	}

	s.logger.Printf("Optimized file uploaded: %s -> %s",
		utils.HumanReadableSize(result.OriginalSize),
		utils.HumanReadableSize(result.ProcessedSize))
	return nil
}

func (s *Service) uploadOriginalFile(ctx context.Context, filePath string) error {
	if err := s.uploader.UploadAsset(ctx, filePath); err != nil {
		return err
	}
	s.logger.Printf("Original file uploaded (no optimization achieved)")
	return nil
}
