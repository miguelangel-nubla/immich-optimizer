package convert

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

type fakeProc struct {
	fn    func(ctx context.Context, path string) (*entity.ProcessResult, error)
	calls atomic.Int32
}

func (f *fakeProc) Process(ctx context.Context, path string, _ []entity.Task) (*entity.ProcessResult, error) {
	f.calls.Add(1)
	return f.fn(ctx, path)
}

type fakeStore struct {
	mu    sync.Mutex
	saves []entity.Failure
	err   error
}

func (s *fakeStore) Save(in entity.ConvertInput, f entity.Failure) (*entity.UndoneRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, f)
	if s.err != nil {
		return nil, s.err
	}
	return &entity.UndoneRecord{Path: "/undone/" + in.Filename, Occurrences: len(s.saves)}, nil
}

func (s *fakeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saves)
}

var jpgTasks = []entity.Task{{Name: "t", Extensions: []string{"jpg"}}}

func quiet() *customlogger.Logger { return customlogger.NewWithLevel(nil, "error", "") }

func input(t *testing.T, size int) entity.ConvertInput {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.jpg")
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return entity.ConvertInput{Path: p, Filename: "orig.JPG", Source: entity.SourceProxy}
}

func output(t *testing.T, name string, size int) (string, func(), *bool) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	_ = os.WriteFile(p, make([]byte, size), 0o600)
	cleaned := new(bool)
	return p, func() { *cleaned = true; _ = os.Remove(p) }, cleaned
}

func TestConvertOptimized(t *testing.T) {
	out, cleanup, cleaned := output(t, "file-123.jxl", 10)
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		return &entity.ProcessResult{ProcessedFilePath: out, ProcessedFilename: "file-123.jxl", OriginalSize: 100, ProcessedSize: 10, Cleanup: cleanup}, nil
	}}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, proc, &fakeStore{}, quiet())
	res, err := e.Convert(context.Background(), input(t, 100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != entity.OutcomeOptimized || res.DeliverName != "orig.jxl" || res.DeliverPath != out {
		t.Fatalf("unexpected result %+v", res)
	}
	res.Release()
	res.Release() // idempotent
	if !*cleaned {
		t.Fatalf("Release did not clean up")
	}
}

func TestConvertNotSmallerIsUnchanged(t *testing.T) {
	out, cleanup, cleaned := output(t, "x.jxl", 200)
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		return &entity.ProcessResult{ProcessedFilePath: out, OriginalSize: 100, ProcessedSize: 200, Cleanup: cleanup}, nil
	}}
	store := &fakeStore{}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, proc, store, quiet())
	in := input(t, 100)
	res, err := e.Convert(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != entity.OutcomeUnchanged || res.DeliverPath != in.Path || res.DeliverName != "orig.JPG" {
		t.Fatalf("unexpected result %+v", res)
	}
	if !*cleaned || store.count() != 0 {
		t.Fatalf("processed output should be released and nothing saved")
	}
	res.Release()
}

func TestConvertEmptyOutputIsFailure(t *testing.T) {
	out, cleanup, cleaned := output(t, "x.jxl", 0)
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		return &entity.ProcessResult{ProcessedFilePath: out, OriginalSize: 100, ProcessedSize: 0, Cleanup: cleanup}, nil
	}}
	store := &fakeStore{}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, proc, store, quiet())
	_, err := e.Convert(context.Background(), input(t, 100))
	var ce *entity.ConversionError
	if !errors.As(err, &ce) || !errors.Is(err, entity.ErrEmptyOutput) {
		t.Fatalf("expected empty-output ConversionError, got %v", err)
	}
	if store.count() != 1 || ce.Undone == nil || !*cleaned {
		t.Fatalf("expected saved to undone and cleaned up")
	}
}

func TestConvertProcessorErrorSaved(t *testing.T) {
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		return nil, &entity.ProcessError{
			Attempts: []entity.TaskAttempt{{Task: "a", ExitCode: 1}, {Task: "b", ExitCode: 2}},
			Err:      errors.New("all failed"),
		}
	}}
	store := &fakeStore{}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, proc, store, quiet())
	_, err := e.Convert(context.Background(), input(t, 100))
	var ce *entity.ConversionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConversionError, got %v", err)
	}
	if ce.Failure.Stage != entity.StageConvert || len(ce.Failure.Attempts) != 2 || store.count() != 1 || ce.Undone == nil {
		t.Fatalf("unexpected failure %+v (saves=%d)", ce, store.count())
	}
}

func TestConvertNoTaskPolicy(t *testing.T) {
	for _, save := range []bool{true, false} {
		proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
			t.Fatalf("processor must not be called")
			return nil, nil
		}}
		store := &fakeStore{}
		e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1, SaveUnmatched: save}, proc, store, quiet())
		in := input(t, 10)
		in.Filename = "doc.pdf"
		_, err := e.Convert(context.Background(), in)
		var ce *entity.ConversionError
		if !errors.As(err, &ce) || !errors.Is(err, entity.ErrNoTask) || ce.Failure.Stage != entity.StageNoTask {
			t.Fatalf("expected no-task ConversionError, got %v", err)
		}
		want := 0
		if save {
			want = 1
		}
		if store.count() != want {
			t.Fatalf("SaveUnmatched=%v: saves=%d", save, store.count())
		}
	}
}

func TestConvertCanceledNotSaved(t *testing.T) {
	started := make(chan struct{})
	proc := &fakeProc{fn: func(ctx context.Context, _ string) (*entity.ProcessResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	store := &fakeStore{}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, proc, store, quiet())
	ctx, cancel := context.WithCancelCause(context.Background())
	go func() { <-started; cancel(entity.ErrShuttingDown) }()
	_, err := e.Convert(ctx, input(t, 10))
	if !errors.Is(err, entity.ErrCanceled) || !errors.Is(err, entity.ErrShuttingDown) {
		t.Fatalf("expected ErrCanceled, got %v", err)
	}
	var ce *entity.ConversionError
	if errors.As(err, &ce) || store.count() != 0 {
		t.Fatalf("cancellation must not save to undone")
	}
}

func TestConvertTaskTimeoutSaved(t *testing.T) {
	proc := &fakeProc{fn: func(ctx context.Context, _ string) (*entity.ProcessResult, error) {
		<-ctx.Done()
		return nil, &entity.ProcessError{Attempts: []entity.TaskAttempt{{Task: "slow"}}, Err: ctx.Err()}
	}}
	store := &fakeStore{}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1, TaskTimeout: 20 * time.Millisecond}, proc, store, quiet())
	_, err := e.Convert(context.Background(), input(t, 10))
	var ce *entity.ConversionError
	if !errors.As(err, &ce) || !errors.Is(err, entity.ErrTaskTimeout) {
		t.Fatalf("expected task-timeout ConversionError, got %v", err)
	}
	if store.count() != 1 || !ce.Failure.Attempts[0].TimedOut {
		t.Fatalf("timeout should be saved and marked: %+v", ce.Failure)
	}
}

func TestConvertCanceledWhileQueued(t *testing.T) {
	block := make(chan struct{})
	started := make(chan struct{}, 1)
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		started <- struct{}{}
		<-block
		return nil, errors.New("x")
	}}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, proc, &fakeStore{}, quiet())
	go func() { _, _ = e.Convert(context.Background(), input(t, 10)) }()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.Convert(ctx, input(t, 10))
	close(block)
	if !errors.Is(err, entity.ErrCanceled) {
		t.Fatalf("expected ErrCanceled while waiting for a slot, got %v", err)
	}
}

func TestConvertConcurrencyLimit(t *testing.T) {
	const limit = 3
	var cur, peak atomic.Int32
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		cur.Add(-1)
		return nil, errors.New("fail")
	}}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: limit}, proc, &fakeStore{}, quiet())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.Convert(context.Background(), input(t, 10))
		}()
	}
	wg.Wait()
	if peak.Load() > limit {
		t.Fatalf("observed %d concurrent conversions, limit %d", peak.Load(), limit)
	}
}

func TestConvertStoreErrorSurfaced(t *testing.T) {
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) { return nil, errors.New("bad") }}
	store := &fakeStore{err: errors.New("disk full")}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, proc, store, quiet())
	_, err := e.Convert(context.Background(), input(t, 10))
	var ce *entity.ConversionError
	if !errors.As(err, &ce) || ce.Undone != nil || ce.UndoneErr == nil {
		t.Fatalf("store error should be in UndoneErr: %+v", ce)
	}
}

func TestSaveUndoneDelegates(t *testing.T) {
	store := &fakeStore{}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1}, &fakeProc{}, store, quiet())
	rec, err := e.SaveUndone(input(t, 1), entity.Failure{Stage: entity.StageUpload, Err: errors.New("503")})
	if err != nil || rec == nil || store.count() != 1 || store.saves[0].At.IsZero() {
		t.Fatalf("SaveUndone: %+v %v", rec, err)
	}
}
