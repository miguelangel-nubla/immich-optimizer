//go:build unix

package processor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

func TestCancelKillsProcessGroup(t *testing.T) {
	tmpDir := t.TempDir()
	in := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(in, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(tmpDir, "grandchild.pid")
	// The grandchild inherits stdout, like HandBrakeCLI under sh -c.
	tasks := []entity.Task{{
		Name:       "slow",
		Extensions: []string{"txt"},
		Command:    "sleep 30 & echo $! > " + pidFile + "; wait",
	}}

	p := NewLocalProcessor(nil, tmpDir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := p.Process(ctx, in, tasks)
		done <- err
	}()

	var pid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		t.Fatalf("grandchild never started")
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected an error after cancel")
		}
	case <-time.After(6 * time.Second):
		t.Fatalf("Process did not return within 6s of cancel")
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("took %s", d)
	}

	// The grandchild must be gone (allow a moment for the kernel to reap it).
	gone := false
	for i := 0; i < 100; i++ {
		if err := syscall.Kill(pid, 0); err != nil {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived cancellation", pid)
	}
}

// A command that leaves a process running in the background (with the output
// still open) succeeds as soon as it exits, and what it left is stopped
// before its result is read: it could still be writing there.
func TestBackgroundLeftoverIsStoppedAfterSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	in := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(in, []byte("a file long enough to be bigger than the converted one"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(tmpDir, "leftover.pid")
	tasks := []entity.Task{{
		Name:       "leaves-a-child",
		Extensions: []string{"txt"},
		Command:    `sleep 30 & echo $! > ` + pidFile + `; printf small > "$IO_DST_FOLDER/$IO_NAME.txt"`,
	}}
	start := time.Now()
	res, err := NewLocalProcessor(nil, tmpDir).Process(context.Background(), in, tasks)
	if err != nil {
		t.Fatalf("a command that exited 0 failed: %v", err)
	}
	defer res.Cleanup()
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("took %s: the leftover's open output was waited for", d)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("the leftover %d is still running", pid)
}
