package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestTasksFile(t *testing.T) string {
	t.Helper()

	tasksFile := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := os.WriteFile(tasksFile, []byte("tasks: []\n"), 0o600); err != nil {
		t.Fatalf("failed to write test tasks file: %v", err)
	}
	return tasksFile
}

func TestParseConfigTuningFlags(t *testing.T) {
	tasksFile := writeTestTasksFile(t)

	cfg, isVersion, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-max_concurrent_requests", "3",
		"-http_timeout_seconds", "45",
		"-inotify_buffer_size", "4096",
	})
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if isVersion {
		t.Fatalf("expected normal config parse, got version path")
	}
	if cfg.MaxConcurrentRequests != 3 {
		t.Fatalf("expected max concurrent requests 3, got %d", cfg.MaxConcurrentRequests)
	}
	if cfg.HTTPTimeoutSeconds != 45 {
		t.Fatalf("expected HTTP timeout 45, got %d", cfg.HTTPTimeoutSeconds)
	}
	if cfg.InotifyBufferSize != 4096 {
		t.Fatalf("expected inotify buffer size 4096, got %d", cfg.InotifyBufferSize)
	}
}

func TestParseConfigRejectsInvalidMaxConcurrentRequests(t *testing.T) {
	tasksFile := writeTestTasksFile(t)

	_, _, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-max_concurrent_requests", "0",
	})
	if err == nil {
		t.Fatalf("expected invalid max_concurrent_requests to fail")
	}
}

func TestParseConfigRejectsInvalidHTTPTimeoutSeconds(t *testing.T) {
	tasksFile := writeTestTasksFile(t)

	_, _, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-http_timeout_seconds", "0",
	})
	if err == nil {
		t.Fatalf("expected invalid http_timeout_seconds to fail")
	}
}

func TestParseConfigRejectsInvalidInotifyBufferSize(t *testing.T) {
	tasksFile := writeTestTasksFile(t)

	_, _, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-inotify_buffer_size", "0",
	})
	if err == nil {
		t.Fatalf("expected invalid inotify_buffer_size to fail")
	}
}
