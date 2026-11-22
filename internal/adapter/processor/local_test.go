package processor

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

func TestLocalProcessor(t *testing.T) {
	tmpDir := t.TempDir()
	watchFile := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(watchFile, []byte("hello world"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	p := NewLocalProcessor(logger, tmpDir)

	tasks := []entity.Task{
		{
			Name:       "copy-task",
			Extensions: []string{"txt"},
			Command:    "cp {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.txt",
		},
	}

	result, err := p.Process(context.Background(), watchFile, tasks)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	defer result.Cleanup()

	if result.ProcessedFilePath == "" {
		t.Errorf("Expected ProcessedFilePath to be set")
	}

	if _, err := os.Stat(result.ProcessedFilePath); err != nil {
		t.Errorf("Expected processed file to exist on disk before Cleanup(), got error: %v", err)
	}
}

func TestLocalProcessorEmptyCommandPassesThrough(t *testing.T) {
	tmpDir := t.TempDir()
	watchFile := filepath.Join(tmpDir, "sample.txt")
	contents := []byte("hello passthrough")
	if err := os.WriteFile(watchFile, contents, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	p := NewLocalProcessor(logger, tmpDir)

	tasks := []entity.Task{
		{
			Name:       "passthrough",
			Extensions: []string{"txt"},
			Command:    "",
		},
	}

	result, err := p.Process(context.Background(), watchFile, tasks)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	defer result.Cleanup()

	got, err := os.ReadFile(result.ProcessedFilePath)
	if err != nil {
		t.Fatalf("failed to read processed file: %v", err)
	}
	if string(got) != string(contents) {
		t.Fatalf("expected passthrough contents %q, got %q", string(contents), string(got))
	}
	if result.ProcessedSize != result.OriginalSize {
		t.Fatalf("expected equal sizes for passthrough, got original=%d processed=%d", result.OriginalSize, result.ProcessedSize)
	}
}

func TestLocalProcessorFailingTasksReportAttempts(t *testing.T) {
	tmpDir := t.TempDir()
	in := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(in, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewLocalProcessor(customlogger.NewWithLevel(nil, "error", ""), tmpDir)
	tasks := []entity.Task{
		// Emits ~100 KiB then fails with exit 3.
		{Name: "first", Extensions: []string{"txt"}, Command: "head -c 100000 /dev/zero | tr '\\0' a; echo END-OF-FIRST; exit 3"},
		{Name: "second", Extensions: []string{"txt"}, Command: "echo second-out >&2; exit 5"},
	}
	_, err := p.Process(context.Background(), in, tasks)
	var pe *entity.ProcessError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *entity.ProcessError, got %T %v", err, err)
	}
	if len(pe.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(pe.Attempts))
	}
	if pe.Attempts[0].ExitCode != 3 || pe.Attempts[1].ExitCode != 5 {
		t.Fatalf("exit codes = %d,%d", pe.Attempts[0].ExitCode, pe.Attempts[1].ExitCode)
	}
	if len(pe.Attempts[0].OutputTail) > OutputTailSize {
		t.Fatalf("tail too long: %d", len(pe.Attempts[0].OutputTail))
	}
	if !strings.Contains(pe.Attempts[0].OutputTail, "END-OF-FIRST") {
		t.Fatalf("tail should keep the end of the output")
	}
	if !strings.Contains(pe.Attempts[1].OutputTail, "second-out") {
		t.Fatalf("stderr not captured: %q", pe.Attempts[1].OutputTail)
	}
	if pe.Attempts[0].Command == "" {
		t.Fatalf("command not recorded")
	}
}

func TestLocalProcessorNoTask(t *testing.T) {
	tmpDir := t.TempDir()
	in := filepath.Join(tmpDir, "sample.pdf")
	_ = os.WriteFile(in, []byte("x"), 0o644)
	p := NewLocalProcessor(nil, tmpDir)
	_, err := p.Process(context.Background(), in, []entity.Task{{Name: "t", Extensions: []string{"jpg"}}})
	if !errors.Is(err, entity.ErrNoTask) {
		t.Fatalf("expected ErrNoTask, got %v", err)
	}
}

func TestTailBufferBoundsMemory(t *testing.T) {
	tb := newTailBuffer(1024)
	chunk := []byte(strings.Repeat("x", 100))
	for i := 0; i < 10000; i++ {
		_, _ = tb.Write(chunk)
	}
	_, _ = tb.Write([]byte("THE-END"))
	if tb.Len() > 2048 {
		t.Fatalf("buffer grew to %d", tb.Len())
	}
	tail := tb.Tail(16)
	if len(tail) > 16 || !strings.HasSuffix(tail, "THE-END") {
		t.Fatalf("tail = %q", tail)
	}
	big := newTailBuffer(8)
	_, _ = big.Write([]byte("0123456789abcdef"))
	if got := big.Tail(100); got != "89abcdef" {
		t.Fatalf("got %q", got)
	}
}

// A command that exits 0 without a usable output has failed: the next task
// is tried.
func TestLocalProcessorBadOutputFallsThrough(t *testing.T) {
	tmpDir := t.TempDir()
	in := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(in, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := NewLocalProcessor(customlogger.NewWithLevel(nil, "error", ""), tmpDir)
	tasks := []entity.Task{
		{Name: "nothing", Extensions: []string{"txt"}, Command: "true"},
		{Name: "two", Extensions: []string{"txt"}, Command: "echo a > {{.dst_folder}}/a.txt; echo b > {{.dst_folder}}/b.txt"},
		{Name: "empty", Extensions: []string{"txt"}, Command: ": > {{.dst_folder}}/a.txt"},
		{Name: "passthrough", Extensions: []string{"txt"}, Command: ""},
	}
	res, err := p.Process(context.Background(), in, tasks)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	if res.Task != "passthrough" || res.ProcessedSize != 7 {
		t.Fatalf("task %q, size %d: the fallback did not run", res.Task, res.ProcessedSize)
	}

	_, err = p.Process(context.Background(), in, tasks[:3])
	var pe *entity.ProcessError
	if !errors.As(err, &pe) || len(pe.Attempts) != 3 {
		t.Fatalf("want 3 failed attempts, got %v", err)
	}
	for _, a := range pe.Attempts {
		if a.ExitCode != 0 || a.Err == "" {
			t.Errorf("attempt %s: exit %d, error %q", a.Task, a.ExitCode, a.Err)
		}
	}
}
