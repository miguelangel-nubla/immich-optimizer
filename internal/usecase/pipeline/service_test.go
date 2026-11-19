package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

type fakeConverter struct {
	mu       sync.Mutex
	convert  func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error)
	saveErr  error
	inputs   []entity.ConvertInput
	saved    []entity.Failure
	released int
}

func (f *fakeConverter) Convert(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	fn := f.convert
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, in)
	}
	return &entity.ConvertResult{
		Outcome:      entity.OutcomeOptimized,
		DeliverPath:  "/tmp/processed.jxl",
		DeliverName:  "processed.jxl",
		OriginalSize: 1000,
		OutputSize:   500,
		Release: func() {
			f.mu.Lock()
			f.released++
			f.mu.Unlock()
		},
	}, nil
}

func (f *fakeConverter) SaveUndone(in entity.ConvertInput, fl entity.Failure) (*entity.UndoneRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = append(f.saved, fl)
	if f.saveErr != nil {
		return nil, f.saveErr
	}
	return &entity.UndoneRecord{Path: "/undone/" + in.Filename, Occurrences: 1}, nil
}

type mockUploader struct {
	mu       sync.Mutex
	failMap  map[string]error
	uploaded []string
	calls    int
}

func (m *mockUploader) UploadAssetWithFilename(ctx context.Context, filePath, filename string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if err, found := m.failMap[filename]; found {
		return err
	}
	m.uploaded = append(m.uploaded, filename)
	return nil
}

type mockFileSystem struct {
	mu      sync.Mutex
	removed []string
}

func (m *mockFileSystem) RemoveFile(filePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed = append(m.removed, filePath)
	return nil
}

func (m *mockFileSystem) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.removed)
}

func quiet() *customlogger.Logger { return customlogger.NewWithLevel(nil, "error", "") }

func touch(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// runEvents feeds paths through a service and drains it.
func runEvents(t *testing.T, svc *Service, events chan entity.FileEvent, paths ...string) {
	t.Helper()
	svc.Start()
	for _, p := range paths {
		events <- entity.FileEvent{Path: p, Timestamp: time.Now()}
	}
	// Wait until every event has been picked up before draining (Drain stops
	// intake; buffered events would otherwise be left for the next start).
	deadline := time.Now().Add(5 * time.Second)
	for len(events) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitIdle(t, svc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// waitIdle waits until no file is being processed.
func waitIdle(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		svc.processing.Range(func(any, any) bool { busy = true; return false })
		if !busy {
			// Give just-dispatched goroutines a chance to register.
			time.Sleep(10 * time.Millisecond)
			svc.processing.Range(func(any, any) bool { busy = true; return false })
			if !busy {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("service did not become idle")
}

func newSvc(conv *fakeConverter, up *mockUploader, fs *mockFileSystem, watchDir string, max int) (*Service, chan entity.FileEvent) {
	events := make(chan entity.FileEvent, 10)
	return NewService(&mockWatcher{events: events}, conv, up, fs, quiet(), max, watchDir), events
}

func TestPipelineSuccessUploadsAndRemoves(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "a.jpg")
	conv, up, fs := &fakeConverter{}, &mockUploader{}, &mockFileSystem{}
	svc, events := newSvc(conv, up, fs, dir, 1)
	runEvents(t, svc, events, f)

	if len(up.uploaded) != 1 || up.uploaded[0] != "processed.jxl" {
		t.Fatalf("uploads = %v", up.uploaded)
	}
	if fs.count() != 1 || conv.released != 1 {
		t.Fatalf("removed=%d released=%d", fs.count(), conv.released)
	}
	in := conv.inputs[0]
	if in.Source != entity.SourceWatcher || in.Filename != "a.jpg" || in.OriginalDir != "" || in.ModTime.IsZero() {
		t.Fatalf("unexpected input %+v", in)
	}
}

func TestPipelineOriginalDirForNestedFiles(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "sub/deeper/a.jpg")
	conv := &fakeConverter{}
	svc, events := newSvc(conv, &mockUploader{}, &mockFileSystem{}, dir, 1)
	runEvents(t, svc, events, f)
	if conv.inputs[0].OriginalDir != filepath.Join("sub", "deeper") {
		t.Fatalf("OriginalDir = %q", conv.inputs[0].OriginalDir)
	}
}

func TestPipelineConversionFailureSavedRemovesSource(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "a.pdf")
	conv := &fakeConverter{convert: func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		return nil, &entity.ConversionError{Filename: in.Filename, Failure: entity.Failure{Stage: entity.StageNoTask, Err: entity.ErrNoTask},
			Undone: &entity.UndoneRecord{Path: "/undone/a.pdf"}}
	}}
	up, fs := &mockUploader{}, &mockFileSystem{}
	svc, events := newSvc(conv, up, fs, dir, 1)
	runEvents(t, svc, events, f)
	if up.calls != 0 {
		t.Fatalf("must not upload on conversion failure")
	}
	if fs.count() != 1 {
		t.Fatalf("source should be removed once saved to undone")
	}
}

func TestPipelineConversionFailureNotSavedKeepsSource(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "a.jpg")
	conv := &fakeConverter{convert: func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		return nil, &entity.ConversionError{Filename: in.Filename, Failure: entity.Failure{Stage: entity.StageConvert, Err: errors.New("x")},
			UndoneErr: errors.New("disk full")}
	}}
	up, fs := &mockUploader{}, &mockFileSystem{}
	svc, events := newSvc(conv, up, fs, dir, 1)
	runEvents(t, svc, events, f)
	if up.calls != 0 || fs.count() != 0 {
		t.Fatalf("source must be kept when it could not be saved (uploads=%d removed=%d)", up.calls, fs.count())
	}
}

func TestPipelineUploadFailureSavesAndRemoves(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "a.jpg")
	conv := &fakeConverter{}
	up := &mockUploader{failMap: map[string]error{"processed.jxl": errors.New("status 400 quota")}}
	fs := &mockFileSystem{}
	svc, events := newSvc(conv, up, fs, dir, 1)
	runEvents(t, svc, events, f)
	if len(conv.saved) != 1 || conv.saved[0].Stage != entity.StageUpload {
		t.Fatalf("expected one upload-stage save, got %+v", conv.saved)
	}
	if fs.count() != 1 {
		t.Fatalf("source should be removed after saving")
	}
}

func TestPipelineUploadFailureSaveFailsKeepsSource(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "a.jpg")
	conv := &fakeConverter{saveErr: errors.New("read-only")}
	up := &mockUploader{failMap: map[string]error{"processed.jxl": errors.New("boom")}}
	fs := &mockFileSystem{}
	svc, events := newSvc(conv, up, fs, dir, 1)
	runEvents(t, svc, events, f)
	if fs.count() != 0 {
		t.Fatalf("source must be kept when the undone save failed")
	}
}

func TestPipelineCanceledDoesNothing(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "a.jpg")
	conv := &fakeConverter{convert: func(context.Context, entity.ConvertInput) (*entity.ConvertResult, error) {
		return nil, entity.ErrCanceled
	}}
	up, fs := &mockUploader{}, &mockFileSystem{}
	svc, events := newSvc(conv, up, fs, dir, 1)
	runEvents(t, svc, events, f)
	if up.calls != 0 || fs.count() != 0 || len(conv.saved) != 0 {
		t.Fatalf("canceled must leave the file alone")
	}
}

func TestPipelineIgnoresUndoneArtifacts(t *testing.T) {
	dir := t.TempDir()
	note := touch(t, dir, "a.jpg.error.json")
	tmp := touch(t, dir, ".io-undone-abc.tmp")
	conv := &fakeConverter{}
	svc, events := newSvc(conv, &mockUploader{}, &mockFileSystem{}, dir, 1)
	runEvents(t, svc, events, note, tmp)
	if len(conv.inputs) != 0 {
		t.Fatalf("undone artifacts must be ignored, got %d conversions", len(conv.inputs))
	}
}

func TestPipelineUploadQuotaExceededContinuesProcessing(t *testing.T) {
	dir := t.TempDir()
	f1, f2, f3 := touch(t, dir, "1.jpg"), touch(t, dir, "2.jpg"), touch(t, dir, "3.jpg")
	conv := &fakeConverter{convert: func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		return &entity.ConvertResult{DeliverPath: in.Path, DeliverName: in.Filename, Release: func() {}}, nil
	}}
	up := &mockUploader{failMap: map[string]error{
		"1.jpg": errors.New(`upload failed with status 400: {"message":"Quota has been exceeded!"}`),
	}}
	fs := &mockFileSystem{}
	svc, events := newSvc(conv, up, fs, dir, 2)
	runEvents(t, svc, events, f1, f2, f3)

	if len(conv.saved) != 1 {
		t.Errorf("expected file 1 saved to undone, got %d", len(conv.saved))
	}
	if len(up.uploaded) != 2 {
		t.Errorf("expected files 2 and 3 uploaded, got %d", len(up.uploaded))
	}
	if fs.count() != 3 {
		t.Errorf("expected all 3 sources removed (1 saved, 2 uploaded), got %d", fs.count())
	}
}

func TestPipelineDrainThenCancel(t *testing.T) {
	dir := t.TempDir()
	f := touch(t, dir, "a.jpg")
	started := make(chan struct{})
	conv := &fakeConverter{convert: func(ctx context.Context, _ entity.ConvertInput) (*entity.ConvertResult, error) {
		close(started)
		<-ctx.Done()
		return nil, entity.ErrCanceled
	}}
	fs := &mockFileSystem{}
	svc, events := newSvc(conv, &mockUploader{}, fs, dir, 1)
	svc.Start()
	events <- entity.FileEvent{Path: f}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := svc.Drain(ctx); err == nil {
		t.Fatalf("drain should time out while a job runs")
	}
	svc.Cancel()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := svc.Drain(ctx2); err != nil {
		t.Fatalf("drain after cancel: %v", err)
	}
	if fs.count() != 0 {
		t.Fatalf("canceled file must stay in place")
	}
}

func TestPipelineDrainDoesNotStartQueuedFiles(t *testing.T) {
	dir := t.TempDir()
	a := touch(t, dir, "a.jpg")
	b := touch(t, dir, "b.jpg")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	conv := &fakeConverter{}
	conv.convert = func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		started <- struct{}{}
		<-release
		return &entity.ConvertResult{Outcome: entity.OutcomeOptimized, DeliverPath: in.Path, DeliverName: in.Filename, Release: func() {}}, nil
	}
	fs := &mockFileSystem{}
	svc, events := newSvc(conv, &mockUploader{}, fs, dir, 1)
	svc.Start()
	events <- entity.FileEvent{Path: a}
	<-started
	events <- entity.FileEvent{Path: b}
	// Wait until b is queued on the concurrency slot.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := 0
		svc.processing.Range(func(any, any) bool { n++; return true })
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("b was never queued")
		}
		time.Sleep(time.Millisecond)
	}

	expired, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_ = svc.Drain(expired) // stops intake
	close(release)
	ctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := svc.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	conv.mu.Lock()
	n := len(conv.inputs)
	conv.mu.Unlock()
	if n != 1 {
		t.Fatalf("queued file was started during shutdown: %d conversions", n)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.removed) != 1 || fs.removed[0] != a {
		t.Fatalf("only a should be removed, got %v", fs.removed)
	}
}
