package convert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
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

type fakeChecker struct {
	got     []string
	err     error
	sizeErr error
}

func (c *fakeChecker) CheckSize(_ context.Context, _, _ string) error { return c.sizeErr }

func (c *fakeChecker) Check(_ context.Context, original, output string) error {
	c.got = []string{original, output}
	return c.err
}

func TestConvertsAndCheckOutput(t *testing.T) {
	tasks := []entity.Task{
		{Name: "jxl", Extensions: []string{"jpg"}, Command: "cjxl"},
		{Name: "pass", Extensions: []string{"heic"}, Command: ""},
	}
	chk := &fakeChecker{}
	e := New(Config{Tasks: tasks, Checker: chk}, &fakeProc{}, &fakeStore{}, quiet())
	for name, want := range map[string]bool{"a.JPG": true, "a.heic": false, "a.xyz": false} {
		ok, reason := e.Converts(name)
		if ok != want || (!ok && reason == "") {
			t.Errorf("Converts(%s) = %v %q, want %v", name, ok, reason, want)
		}
	}
	in := entity.ConvertInput{Path: "/in/a.jpg", Filename: "a.jpg"}
	res := &entity.ConvertResult{Outcome: entity.OutcomeOptimized, DeliverPath: "/out/a.jxl"}
	if err := e.CheckOutput(context.Background(), in, res); err != nil || len(chk.got) != 2 || chk.got[0] != "/in/a.jpg" || chk.got[1] != "/out/a.jxl" {
		t.Errorf("CheckOutput: %v %v", err, chk.got)
	}
	chk.err = errors.New("flat")
	if err := e.CheckOutput(context.Background(), in, res); err == nil {
		t.Error("a failed check must be an error")
	}
	if err := e.CheckOutput(context.Background(), in, &entity.ConvertResult{Outcome: entity.OutcomeUnchanged}); err == nil {
		t.Error("an unconverted result must not pass")
	}
	if err := New(Config{Tasks: tasks}, &fakeProc{}, &fakeStore{}, quiet()).CheckOutput(context.Background(), in, res); err == nil {
		t.Error("no checker configured must not pass")
	}
}

func TestSkipUndoneDoesNotSaveFailedInput(t *testing.T) {
	in := input(t, 100)
	in.SkipUndone = true
	store := &fakeStore{}
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) { return nil, errors.New("boom") }}
	if _, err := New(Config{Tasks: jpgTasks}, proc, store, quiet()).Convert(context.Background(), in); err == nil {
		t.Fatal("expected an error")
	}
	if store.count() != 0 {
		t.Errorf("undone saves = %d, want 0", store.count())
	}
}

func TestEngineAuditsFailedAndRefusedInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) { return nil, errors.New("boom") }}
	e := New(Config{Tasks: jpgTasks, SaveUnmatched: true, Audit: l}, proc, &fakeStore{}, quiet())

	in := input(t, 100)
	_, _ = e.Convert(context.Background(), in) // conversion fails
	xyz := in
	xyz.Filename = "note.xyz"
	_, _ = e.Convert(context.Background(), xyz) // no task
	lib := in
	lib.Source, lib.SkipUndone = entity.SourceLibrary, true
	_, _ = e.Convert(context.Background(), lib) // library mode records its own entries

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var entry audit.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Source != "proxy" {
			t.Errorf("entry = %+v", entry)
		}
		events = append(events, entry.Event+":"+entry.File)
	}
	if len(events) != 2 || events[0] != "failed:orig.JPG" || events[1] != "refused:note.xyz" {
		t.Errorf("audit events = %v", events)
	}
}

// No audit log, no work: the engine refuses before touching the file.
func TestEngineRefusesWorkWhenAuditLogIsBroken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := audit.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		t.Error("the task ran although the audit log is broken")
		return nil, errors.New("unreachable")
	}}
	store := &fakeStore{}
	e := New(Config{Tasks: jpgTasks, Audit: l}, proc, store, quiet())
	l.Close() // every write now fails
	_ = l.Write(audit.Entry{Event: "x"})
	if _, err := e.Convert(context.Background(), input(t, 100)); !errors.Is(err, entity.ErrAuditUnavailable) {
		t.Fatalf("err = %v, want ErrAuditUnavailable", err)
	}
	if store.count() != 0 {
		t.Error("something was saved to undone")
	}
}

// A result that is tiny next to its original is not delivered, in any mode:
// the original is saved like that of any failed conversion.
func TestConvertRefusesATinyResult(t *testing.T) {
	out, cleanup, cleaned := output(t, "file-123.jxl", 10)
	proc := &fakeProc{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		return &entity.ProcessResult{ProcessedFilePath: out, ProcessedFilename: "file-123.jxl", OriginalSize: 100, ProcessedSize: 10, Cleanup: cleanup}, nil
	}}
	store := &fakeStore{}
	chk := &fakeChecker{sizeErr: errors.New("160x120 against 6000x4000")}
	e := New(Config{Tasks: jpgTasks, MaxConcurrent: 1, Checker: chk}, proc, store, quiet())
	_, err := e.Convert(context.Background(), input(t, 100))
	var ce *entity.ConversionError
	if !errors.As(err, &ce) || ce.Undone == nil || store.count() != 1 || !*cleaned {
		t.Fatalf("err = %v, saved %d, cleaned %v", err, store.count(), *cleaned)
	}
	if !strings.Contains(ce.Failure.Err.Error(), "160x120") {
		t.Errorf("the failure does not say why: %v", ce.Failure.Err)
	}

	chk.sizeErr = nil
	out2, cleanup2, _ := output(t, "file-124.jxl", 10)
	proc.fn = func(context.Context, string) (*entity.ProcessResult, error) {
		return &entity.ProcessResult{ProcessedFilePath: out2, ProcessedFilename: "file-124.jxl", OriginalSize: 100, ProcessedSize: 10, Cleanup: cleanup2}, nil
	}
	if res, err := e.Convert(context.Background(), input(t, 100)); err != nil || res.Outcome != entity.OutcomeOptimized {
		t.Fatalf("a result of a sensible size was refused: %v", err)
	}
}
