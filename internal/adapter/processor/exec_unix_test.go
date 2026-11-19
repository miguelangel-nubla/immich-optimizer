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
