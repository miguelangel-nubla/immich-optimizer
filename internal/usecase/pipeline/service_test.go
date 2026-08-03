package pipeline

import (
	"context"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

type mockWatcher struct {
	events chan entity.FileEvent
}

func (m *mockWatcher) Events() <-chan entity.FileEvent { return m.events }
func (m *mockWatcher) Start() error                    { return nil }
func (m *mockWatcher) Stop() error                     { return nil }

type mockProcessor struct {
	processFunc func(filePath string) (*entity.ProcessResult, error)
}

func (m *mockProcessor) Process(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error) {
	if m.processFunc != nil {
		return m.processFunc(filePath)
	}
	return &entity.ProcessResult{
		ProcessedFilePath: "/tmp/processed.jpg",
		ProcessedFilename: "processed.jpg",
		OriginalSize:      1000,
		ProcessedSize:     500,
	}, nil
}

type mockUploader struct {
	uploadedOriginal  []string
	uploadedProcessed []string
	sync.Mutex
}

func (m *mockUploader) UploadAsset(ctx context.Context, filePath string) error {
	m.Lock()
	defer m.Unlock()
	m.uploadedOriginal = append(m.uploadedOriginal, filePath)
	return nil
}

func (m *mockUploader) UploadAssetWithFilename(ctx context.Context, filePath, filename string) error {
	m.Lock()
	defer m.Unlock()
	m.uploadedProcessed = append(m.uploadedProcessed, filename)
	return nil
}

type mockFileSystem struct {
	removed []string
	undone  []string
	sync.Mutex
}

func (m *mockFileSystem) MoveToUndone(filePath string) error {
	m.Lock()
	defer m.Unlock()
	m.undone = append(m.undone, filePath)
	return nil
}
func (m *mockFileSystem) RemoveFile(filePath string) error {
	m.Lock()
	defer m.Unlock()
	m.removed = append(m.removed, filePath)
	return nil
}

func TestPipelineService(t *testing.T) {
	events := make(chan entity.FileEvent, 10)
	watcher := &mockWatcher{events: events}
	processor := &mockProcessor{}
	uploader := &mockUploader{}
	fs := &mockFileSystem{}
	logger := customlogger.New(log.New(os.Stdout, "", 0), "")

	tasks := []entity.Task{
		{
			Name:       "TestTask",
			Extensions: []string{"jpg"},
		},
	}

	service := NewService(watcher, processor, uploader, fs, logger, tasks, 1)

	// We create a dummy test file so validateFile succeeds
	f, _ := os.CreateTemp("", "test_event*.jpg")
	f.Close()
	defer os.Remove(f.Name())

	service.Start()

	events <- entity.FileEvent{
		Path:      f.Name(),
		Timestamp: time.Now(),
	}

	time.Sleep(100 * time.Millisecond) // Give the pipeline time to process

	service.Stop()

	uploader.Lock()
	defer uploader.Unlock()

	if len(uploader.uploadedProcessed) != 1 {
		t.Errorf("Expected 1 processed upload, got %d", len(uploader.uploadedProcessed))
	}
	if uploader.uploadedProcessed[0] != "processed.jpg" {
		t.Errorf("Expected uploaded filename to be 'processed.jpg', got %s", uploader.uploadedProcessed[0])
	}

	fs.Lock()
	defer fs.Unlock()
	if len(fs.removed) != 1 {
		t.Errorf("Expected 1 file removal (cleanup), got %d", len(fs.removed))
	}
}

func TestPipelineServiceBlocksUnmatchedExtension(t *testing.T) {
	events := make(chan entity.FileEvent, 10)
	watcher := &mockWatcher{events: events}
	processor := &mockProcessor{
		processFunc: func(filePath string) (*entity.ProcessResult, error) {
			t.Fatalf("processor should not be called for unmatched extension")
			return nil, nil
		},
	}
	uploader := &mockUploader{}
	fs := &mockFileSystem{}
	logger := customlogger.New(log.New(os.Stdout, "", 0), "")

	tasks := []entity.Task{
		{
			Name:       "TestTask",
			Extensions: []string{"jpg"},
		},
	}

	service := NewService(watcher, processor, uploader, fs, logger, tasks, 1)

	f, _ := os.CreateTemp("", "test_event*.pdf")
	f.Close()
	defer os.Remove(f.Name())

	service.Start()

	events <- entity.FileEvent{
		Path:      f.Name(),
		Timestamp: time.Now(),
	}

	time.Sleep(100 * time.Millisecond)

	service.Stop()

	uploader.Lock()
	if len(uploader.uploadedOriginal) != 0 || len(uploader.uploadedProcessed) != 0 {
		t.Fatalf("expected no uploads for unmatched extension, got original=%d processed=%d", len(uploader.uploadedOriginal), len(uploader.uploadedProcessed))
	}
	uploader.Unlock()

	fs.Lock()
	defer fs.Unlock()
	if len(fs.undone) != 1 {
		t.Fatalf("expected unmatched file to be moved to undone, got %d", len(fs.undone))
	}
}

func TestShouldOptimizeFileExtensionNormalization(t *testing.T) {
	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	tasks := []entity.Task{
		{
			Name:       "TestTask",
			Extensions: []string{".JPG", "png"},
		},
	}

	service := NewService(nil, nil, nil, nil, logger, tasks, 1)

	if !service.shouldOptimizeFile("/path/to/image.jpg") {
		t.Errorf("expected shouldOptimizeFile to return true for /path/to/image.jpg")
	}
	if !service.shouldOptimizeFile("/path/to/image.PNG") {
		t.Errorf("expected shouldOptimizeFile to return true for /path/to/image.PNG")
	}
	if service.shouldOptimizeFile("/path/to/image.gif") {
		t.Errorf("expected shouldOptimizeFile to return false for /path/to/image.gif")
	}
}

func TestContextCanceledDoesNotMoveToUndone(t *testing.T) {
	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	fs := &mockFileSystem{}
	service := NewService(nil, nil, nil, fs, logger, nil, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	service.handleProcessingError(ctx, "/path/to/canceled_file.jpg", ctx.Err())

	fs.Lock()
	defer fs.Unlock()
	if len(fs.undone) != 0 {
		t.Errorf("Expected 0 files moved to undone on context cancel, got %d", len(fs.undone))
	}
}

func TestShouldUploadProcessedFileNil(t *testing.T) {
	service := NewService(nil, nil, nil, nil, nil, nil, 1)
	if service.shouldUploadProcessedFile(nil) {
		t.Errorf("Expected shouldUploadProcessedFile(nil) to return false, got true")
	}
}
