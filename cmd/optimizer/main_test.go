package main

import (
	"os"
	"path/filepath"
	"strings"
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

const (
	testAPIKeyEnv  = "dummy-env-api-key-0000"
	testAPIKeyFile = "dummy-file-api-key-1111"
)

func writeTestSecretFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "immich_api_key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write test secret file: %v", err)
	}
	return path
}

func parseAPIKeyTestConfig(t *testing.T, extraArgs ...string) (string, error) {
	t.Helper()

	args := append([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", writeTestTasksFile(t),
	}, extraArgs...)
	cfg, _, err := parseConfig(args)
	if err != nil {
		return "", err
	}
	return cfg.ImmichAPIKey, nil
}

func TestParseConfigAPIKeyFromFileOnly(t *testing.T) {
	t.Setenv("IUO_IMMICH_API_KEY", "")
	t.Setenv("IUO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, testAPIKeyFile))

	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyFile {
		t.Fatalf("expected API key from file, got a different value")
	}
}

func TestParseConfigAPIKeyFromEnvOnly(t *testing.T) {
	t.Setenv("IUO_IMMICH_API_KEY", testAPIKeyEnv)
	t.Setenv("IUO_IMMICH_API_KEY_FILE", "")

	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyEnv {
		t.Fatalf("expected API key from env, got a different value")
	}
}

func TestParseConfigAPIKeyFileWinsOverEnv(t *testing.T) {
	t.Setenv("IUO_IMMICH_API_KEY", testAPIKeyEnv)
	t.Setenv("IUO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, testAPIKeyFile))

	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyFile {
		t.Fatalf("expected API key file to take precedence over env")
	}
}

func TestParseConfigAPIKeyFileFlag(t *testing.T) {
	t.Setenv("IUO_IMMICH_API_KEY", "")
	t.Setenv("IUO_IMMICH_API_KEY_FILE", "")

	key, err := parseAPIKeyTestConfig(t,
		"-immich_api_key", testAPIKeyEnv,
		"-immich_api_key_file", writeTestSecretFile(t, testAPIKeyFile+"\n"),
	)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyFile {
		t.Fatalf("expected -immich_api_key_file to take precedence over -immich_api_key")
	}
}

func TestParseConfigAPIKeyFileTrailingNewlineStripped(t *testing.T) {
	for name, content := range map[string]string{
		"lf":   testAPIKeyFile + "\n",
		"crlf": testAPIKeyFile + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("IUO_IMMICH_API_KEY", "")
			t.Setenv("IUO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, content))

			key, err := parseAPIKeyTestConfig(t)
			if err != nil {
				t.Fatalf("parseConfig failed: %v", err)
			}
			if key != testAPIKeyFile {
				t.Fatalf("expected trailing newline to be stripped, got %q", key)
			}
		})
	}

	// Only one trailing newline is stripped.
	t.Setenv("IUO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, testAPIKeyFile+"\n\n"))
	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyFile+"\n" {
		t.Fatalf("expected exactly one trailing newline to be stripped, got %q", key)
	}
}

func TestParseConfigAPIKeyFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("IUO_IMMICH_API_KEY", testAPIKeyEnv)
	t.Setenv("IUO_IMMICH_API_KEY_FILE", missing)

	_, err := parseAPIKeyTestConfig(t)
	if err == nil {
		t.Fatalf("expected missing API key file to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "IUO_IMMICH_API_KEY_FILE") || !strings.Contains(msg, missing) {
		t.Fatalf("expected error to name the variable and path, got: %v", err)
	}
	if strings.Contains(msg, testAPIKeyEnv) {
		t.Fatalf("error must not contain the secret value")
	}
}

func TestParseConfigAPIKeyFileMissingViaFlag(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("IUO_IMMICH_API_KEY_FILE", "")

	_, err := parseAPIKeyTestConfig(t, "-immich_api_key_file", missing)
	if err == nil || !strings.Contains(err.Error(), "-immich_api_key_file") {
		t.Fatalf("expected error naming -immich_api_key_file, got: %v", err)
	}
}

func TestParseConfigAPIKeyFileEmpty(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "newline-only": "\n"} {
		t.Run(name, func(t *testing.T) {
			path := writeTestSecretFile(t, content)
			t.Setenv("IUO_IMMICH_API_KEY", testAPIKeyEnv)
			t.Setenv("IUO_IMMICH_API_KEY_FILE", path)

			_, err := parseAPIKeyTestConfig(t)
			if err == nil {
				t.Fatalf("expected empty API key file to fail")
			}
			if !strings.Contains(err.Error(), "IUO_IMMICH_API_KEY_FILE") || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected error to name the variable and path, got: %v", err)
			}
		})
	}
}
