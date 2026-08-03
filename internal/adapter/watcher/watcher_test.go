package watcher

import (
	"log"
	"os"
	"runtime"
	"testing"

	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

func TestNewInotifyWatcher(t *testing.T) {
	tempDir := t.TempDir()
	logger := customlogger.New(log.New(os.Stdout, "", 0), "")

	w, err := NewInotifyWatcher(tempDir, logger, 4096)
	if runtime.GOOS == "linux" {
		if err != nil {
			t.Fatalf("expected NewInotifyWatcher to succeed on linux, got: %v", err)
		}
		if w == nil {
			t.Fatalf("expected non-nil watcher on linux")
		}
		defer w.Stop()
	} else {
		if err == nil {
			t.Fatalf("expected error on non-linux OS (%s), got nil", runtime.GOOS)
		}
	}
}
