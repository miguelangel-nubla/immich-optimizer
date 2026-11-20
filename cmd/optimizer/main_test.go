package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

// clearConfigEnv blanks every IO_* and IUO_* variable so the developer's
// shell cannot leak into tests (empty counts as unset).
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, name := range allEnvNames() {
		t.Setenv(name, "")
	}
}

func writeTestTasksFile(t *testing.T) string {
	t.Helper()

	t.Setenv("IO_UNDONE_DIR", filepath.Join(t.TempDir(), "undone"))
	tasksFile := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := os.WriteFile(tasksFile, []byte("tasks: []\n"), 0o600); err != nil {
		t.Fatalf("failed to write test tasks file: %v", err)
	}
	return tasksFile
}

func TestParseConfigTuningFlags(t *testing.T) {
	clearConfigEnv(t)
	tasksFile := writeTestTasksFile(t)

	cfg, _, err := parseConfig([]string{
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
	if cfg.ShowVersion {
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
	clearConfigEnv(t)
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
	clearConfigEnv(t)
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
	clearConfigEnv(t)
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
		"-undone_dir", filepath.Join(t.TempDir(), "undone"),
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
	clearConfigEnv(t)
	t.Setenv("IO_IMMICH_API_KEY", "")
	t.Setenv("IO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, testAPIKeyFile))

	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyFile {
		t.Fatalf("expected API key from file, got a different value")
	}
}

func TestParseConfigAPIKeyFromEnvOnly(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("IO_IMMICH_API_KEY", testAPIKeyEnv)
	t.Setenv("IO_IMMICH_API_KEY_FILE", "")

	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyEnv {
		t.Fatalf("expected API key from env, got a different value")
	}
}

func TestParseConfigAPIKeyFileWinsOverEnv(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("IO_IMMICH_API_KEY", testAPIKeyEnv)
	t.Setenv("IO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, testAPIKeyFile))

	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyFile {
		t.Fatalf("expected API key file to take precedence over env")
	}
}

func TestParseConfigAPIKeyFileFlag(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("IO_IMMICH_API_KEY", "")
	t.Setenv("IO_IMMICH_API_KEY_FILE", "")

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
	clearConfigEnv(t)
	for name, content := range map[string]string{
		"lf":   testAPIKeyFile + "\n",
		"crlf": testAPIKeyFile + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("IO_IMMICH_API_KEY", "")
			t.Setenv("IO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, content))

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
	t.Setenv("IO_IMMICH_API_KEY_FILE", writeTestSecretFile(t, testAPIKeyFile+"\n\n"))
	key, err := parseAPIKeyTestConfig(t)
	if err != nil {
		t.Fatalf("parseConfig failed: %v", err)
	}
	if key != testAPIKeyFile+"\n" {
		t.Fatalf("expected exactly one trailing newline to be stripped, got %q", key)
	}
}

func TestParseConfigAPIKeyFileMissing(t *testing.T) {
	clearConfigEnv(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("IO_IMMICH_API_KEY", testAPIKeyEnv)
	t.Setenv("IO_IMMICH_API_KEY_FILE", missing)

	_, err := parseAPIKeyTestConfig(t)
	if err == nil {
		t.Fatalf("expected missing API key file to fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "IO_IMMICH_API_KEY_FILE") || !strings.Contains(msg, missing) {
		t.Fatalf("expected error to name the variable and path, got: %v", err)
	}
	if strings.Contains(msg, testAPIKeyEnv) {
		t.Fatalf("error must not contain the secret value")
	}
}

func TestParseConfigAPIKeyFileMissingViaFlag(t *testing.T) {
	clearConfigEnv(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("IO_IMMICH_API_KEY_FILE", "")

	_, err := parseAPIKeyTestConfig(t, "-immich_api_key_file", missing)
	if err == nil || !strings.Contains(err.Error(), "-immich_api_key_file") {
		t.Fatalf("expected error naming -immich_api_key_file, got: %v", err)
	}
}

func TestParseConfigAPIKeyFileEmpty(t *testing.T) {
	clearConfigEnv(t)
	for name, content := range map[string]string{"empty": "", "newline-only": "\n"} {
		t.Run(name, func(t *testing.T) {
			path := writeTestSecretFile(t, content)
			t.Setenv("IO_IMMICH_API_KEY", testAPIKeyEnv)
			t.Setenv("IO_IMMICH_API_KEY_FILE", path)

			_, err := parseAPIKeyTestConfig(t)
			if err == nil {
				t.Fatalf("expected empty API key file to fail")
			}
			if !strings.Contains(err.Error(), "IO_IMMICH_API_KEY_FILE") || !strings.Contains(err.Error(), path) {
				t.Fatalf("expected error to name the variable and path, got: %v", err)
			}
		})
	}
}

// ---- IO_ prefix, legacy IUO_ fallback, deprecated flags ----

func baseArgs(t *testing.T) []string {
	t.Helper()
	return []string{"-tasks_file", writeTestTasksFile(t)}
}

func TestEnvIOPrefix(t *testing.T) {
	clearConfigEnv(t)
	tasks := writeTestTasksFile(t)
	undoneDir := filepath.Join(t.TempDir(), "u")
	watchDir := filepath.Join(t.TempDir(), "w")
	env := map[string]string{
		"IO_MODE":                        "watcher,proxy",
		"IO_IMMICH_URL":                  "http://io.example:2283",
		"IO_IMMICH_API_KEY":              testAPIKeyEnv,
		"IO_TASKS_FILE":                  tasks,
		"IO_LOG_LEVEL":                   "debug",
		"IO_MAX_CONCURRENT_REQUESTS":     "4",
		"IO_HTTP_TIMEOUT_SECONDS":        "33",
		"IO_TASK_TIMEOUT_SECONDS":        "600",
		"IO_SHUTDOWN_TIMEOUT_SECONDS":    "20",
		"IO_UNDONE_DIR":                  undoneDir,
		"IO_UNDONE_UNMATCHED":            "false",
		"IO_PROXY_BIND_ADDR":             ":9999",
		"IO_PROXY_FILTER_PATH":           "/x",
		"IO_PROXY_FILTER_FORM_KEY":       "file",
		"IO_WATCHER_WATCH_DIR":           watchDir,
		"IO_WATCHER_INOTIFY_BUFFER_SIZE": "1024",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, deps, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("expected no deprecations, got %+v", deps)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"mode", cfg.Mode, "watcher,proxy"},
		{"immich_url", cfg.ImmichURL, "http://io.example:2283"},
		{"api key", cfg.ImmichAPIKey, testAPIKeyEnv},
		{"tasks", cfg.ConfigFile, tasks},
		{"log", cfg.LogLevel, "debug"},
		{"max", cfg.MaxConcurrentRequests, 4},
		{"http", cfg.HTTPTimeoutSeconds, 33},
		{"task timeout", cfg.TaskTimeoutSeconds, 600},
		{"shutdown", cfg.ShutdownTimeoutSeconds, 20},
		{"undone", cfg.UndoneDir, undoneDir},
		{"undone unmatched", cfg.UndoneUnmatched, false},
		{"bind", cfg.BindAddr, ":9999"},
		{"filter path", cfg.FilterPath, "/x"},
		{"form key", cfg.FilterFormKey, "file"},
		{"watch", cfg.WatchDir, watchDir},
		{"inotify", cfg.InotifyBufferSize, 1024},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestEnvLegacyIUOFallback(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("IUO_IMMICH_URL", "http://legacy.example")
	cfg, deps, err := parseConfig(append(baseArgs(t), "-mode", "proxy"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ImmichURL != "http://legacy.example" {
		t.Fatalf("legacy value not used: %q", cfg.ImmichURL)
	}
	if len(deps) != 1 || deps[0] != (deprecation{Old: "IUO_IMMICH_URL", New: "IO_IMMICH_URL"}) {
		t.Fatalf("unexpected deprecations: %+v", deps)
	}
}

func TestEnvIOWinsOverIUO(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("IO_IMMICH_URL", "http://new.example")
	t.Setenv("IUO_IMMICH_URL", "http://old.example")
	cfg, deps, err := parseConfig(append(baseArgs(t), "-mode", "proxy"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ImmichURL != "http://new.example" {
		t.Fatalf("IO_ should win, got %q", cfg.ImmichURL)
	}
	if len(deps) != 1 || !deps[0].Ignored || deps[0].Old != "IUO_IMMICH_URL" {
		t.Fatalf("expected ignored deprecation, got %+v", deps)
	}
	if !strings.Contains(formatDeprecations(deps), "ignored, IO_IMMICH_URL is set") {
		t.Fatalf("warning should say the legacy var was ignored: %s", formatDeprecations(deps))
	}
}

func TestEnvLegacyOrder(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("IUO_IMMICH_URL", "http://immich.example")
	t.Setenv("IUO_PROXY_BIND_ADDR", ":1111")
	t.Setenv("IUO_LISTEN", ":2222")
	cfg, deps, err := parseConfig(append(baseArgs(t), "-mode", "proxy"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.BindAddr != ":1111" {
		t.Fatalf("IUO_PROXY_BIND_ADDR should beat IUO_LISTEN, got %q", cfg.BindAddr)
	}
	var listen *deprecation
	for i := range deps {
		if deps[i].Old == "IUO_LISTEN" {
			listen = &deps[i]
		}
	}
	if listen == nil || !listen.Ignored || listen.Winner != "IUO_PROXY_BIND_ADDR" {
		t.Fatalf("IUO_LISTEN should be reported as ignored: %+v", deps)
	}
}

func TestEnvLegacyUpstreamListen(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("IUO_UPSTREAM", "http://upstream.example")
	t.Setenv("IUO_LISTEN", ":3000")
	cfg, deps, err := parseConfig(append(baseArgs(t), "-mode", "proxy"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ImmichURL != "http://upstream.example" || cfg.BindAddr != ":3000" {
		t.Fatalf("legacy upstream/listen not applied: %q %q", cfg.ImmichURL, cfg.BindAddr)
	}
	msg := formatDeprecations(deps)
	if !strings.Contains(msg, "IUO_UPSTREAM -> IO_IMMICH_URL") || !strings.Contains(msg, "IUO_LISTEN -> IO_PROXY_BIND_ADDR") {
		t.Fatalf("unexpected warning: %s", msg)
	}
}

func TestEnvLegacyUndoneAliases(t *testing.T) {
	for _, name := range []string{"IUO_UNDONE_DIR", "IUO_WATCHER_UNDONE_DIR"} {
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			dir := filepath.Join(t.TempDir(), "legacy-undone")
			t.Setenv("IO_IMMICH_URL", "http://immich.example")
			tasks := writeTestTasksFile(t) // sets IO_UNDONE_DIR
			t.Setenv("IO_UNDONE_DIR", "")
			t.Setenv(name, dir)
			cfg, deps, err := parseConfig([]string{"-mode", "proxy", "-tasks_file", tasks})
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if cfg.UndoneDir != dir {
				t.Fatalf("got undone dir %q, want %q", cfg.UndoneDir, dir)
			}
			if len(deps) != 1 || deps[0].New != "IO_UNDONE_DIR" {
				t.Fatalf("unexpected deprecations %+v", deps)
			}
		})
	}
}

func TestDeprecatedFlags(t *testing.T) {
	clearConfigEnv(t)
	cfg, deps, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-upstream", "http://flag.example", "-listen", ":4000"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ImmichURL != "http://flag.example" || cfg.BindAddr != ":4000" {
		t.Fatalf("deprecated flags not applied: %q %q", cfg.ImmichURL, cfg.BindAddr)
	}
	if len(deps) != 2 || deps[0].Old != "-upstream" || deps[1].Old != "-listen" {
		t.Fatalf("unexpected deprecations %+v", deps)
	}

	// New flag beats deprecated flag.
	cfg, deps, err = parseConfig(append(baseArgs(t), "-mode", "proxy", "-upstream", "http://old.example", "-immich_url", "http://new.example"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ImmichURL != "http://new.example" || len(deps) != 1 || !deps[0].Ignored {
		t.Fatalf("-immich_url should win: %q %+v", cfg.ImmichURL, deps)
	}

	// Deprecated flag beats env.
	t.Setenv("IO_IMMICH_URL", "http://env.example")
	cfg, _, err = parseConfig(append(baseArgs(t), "-mode", "proxy", "-upstream", "http://flag.example"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.ImmichURL != "http://flag.example" {
		t.Fatalf("flag should beat env, got %q", cfg.ImmichURL)
	}
}

func TestHiddenFlagsNotInUsage(t *testing.T) {
	clearConfigEnv(t)
	var buf strings.Builder
	old := flagOutput
	flagOutput = &buf
	defer func() { flagOutput = old }()

	if _, _, err := parseConfig([]string{"-h"}); err == nil {
		t.Fatalf("expected flag.ErrHelp")
	}
	out := buf.String()
	if !strings.Contains(out, "-immich_url") || !strings.Contains(out, "-undone_dir") {
		t.Fatalf("usage missing regular flags:\n%s", out)
	}
	if strings.Contains(out, "-upstream") || strings.Contains(out, "-listen") {
		t.Fatalf("usage must not list hidden flags:\n%s", out)
	}
}

func TestFormatDeprecationsSingleLine(t *testing.T) {
	msg := formatDeprecations([]deprecation{
		{Old: "IUO_IMMICH_URL", New: "IO_IMMICH_URL"},
		{Old: "IUO_LISTEN", New: "IO_PROXY_BIND_ADDR", Ignored: true, Winner: "IO_PROXY_BIND_ADDR"},
		{Old: "-upstream", New: "-immich_url"},
	})
	if strings.Contains(msg, "\n") {
		t.Fatalf("warning must be a single line: %q", msg)
	}
	want := "IUO_IMMICH_URL -> IO_IMMICH_URL; IUO_LISTEN -> IO_PROXY_BIND_ADDR (ignored, IO_PROXY_BIND_ADDR is set); -upstream -> -immich_url"
	if !strings.HasSuffix(msg, want) {
		t.Fatalf("got %q", msg)
	}
	if formatDeprecations(nil) != "" {
		t.Fatalf("no deprecations must give empty string")
	}
}

func TestDefaultBindAddr2283(t *testing.T) {
	clearConfigEnv(t)
	cfg, _, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.BindAddr != ":2283" {
		t.Fatalf("default bind addr = %q, want :2283", cfg.BindAddr)
	}
	if !cfg.UndoneUnmatched || cfg.TaskTimeoutSeconds != 0 || cfg.ShutdownTimeoutSeconds != 8 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

type fakeInfo struct {
	os.FileInfo
	dir bool
}

func (f fakeInfo) IsDir() bool { return f.dir }

func TestTasksFileSearchList(t *testing.T) {
	stat := func(existing ...string) func(string) (os.FileInfo, error) {
		return func(p string) (os.FileInfo, error) {
			for _, e := range existing {
				if e == p {
					return fakeInfo{}, nil
				}
			}
			return nil, os.ErrNotExist
		}
	}
	if got, _ := resolveTasksFile("/x.yaml", "", stat()); got != "/x.yaml" {
		t.Fatalf("explicit path must win, got %q", got)
	}
	if got, _ := resolveTasksFile("", "", stat("tasks.yaml", defaultTasksFile)); got != "tasks.yaml" {
		t.Fatalf("./tasks.yaml should be preferred, got %q", got)
	}
	if got, _ := resolveTasksFile("", "", stat(defaultTasksFile)); got != defaultTasksFile {
		t.Fatalf("image default expected, got %q", got)
	}
	_, err := resolveTasksFile("", "", stat())
	if err == nil || !strings.Contains(err.Error(), "tasks.yaml") || !strings.Contains(err.Error(), defaultTasksFile) {
		t.Fatalf("error should name both paths, got %v", err)
	}
}

func TestUndoneDirCreatedInProxyMode(t *testing.T) {
	clearConfigEnv(t)
	dir := filepath.Join(t.TempDir(), "nested", "undone")
	_, _, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example", "-undone_dir", dir))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("undone dir not created in proxy mode: %v", err)
	}
}

func TestUndoneDirNotWritableRejected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	clearConfigEnv(t)
	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(ro, 0o700)
	_, _, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example", "-undone_dir", ro))
	if err == nil || !strings.Contains(err.Error(), "not writable by uid") {
		t.Fatalf("expected not-writable error, got %v", err)
	}
}

func TestUndoneInsideWatchRejected(t *testing.T) {
	clearConfigEnv(t)
	watch := t.TempDir()
	for _, undoneDir := range []string{watch, filepath.Join(watch, "undone")} {
		_, _, err := parseConfig(append(baseArgs(t),
			"-mode", "watcher", "-immich_url", "http://immich.example", "-immich_api_key", testAPIKeyEnv,
			"-watch_dir", watch, "-undone_dir", undoneDir))
		if err == nil || !strings.Contains(err.Error(), "inside") {
			t.Fatalf("undone %q inside watch %q should be rejected, got %v", undoneDir, watch, err)
		}
	}
	// A sibling whose name merely starts with the watch dir name is fine.
	_, _, err := parseConfig(append(baseArgs(t),
		"-mode", "watcher", "-immich_url", "http://immich.example", "-immich_api_key", testAPIKeyEnv,
		"-watch_dir", watch, "-undone_dir", watch+"-undone"))
	if err != nil {
		t.Fatalf("sibling undone dir rejected: %v", err)
	}
	os.RemoveAll(watch + "-undone")
}

func TestNegativeTaskTimeoutRejected(t *testing.T) {
	clearConfigEnv(t)
	for _, flagName := range []string{"-task_timeout_seconds", "-shutdown_timeout_seconds"} {
		_, _, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example", flagName, "-1"))
		if err == nil {
			t.Fatalf("%s=-1 should be rejected", flagName)
		}
	}
}

func TestAPIKeyFileErrorNamesLegacyVar(t *testing.T) {
	clearConfigEnv(t)
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("IUO_IMMICH_API_KEY_FILE", missing)
	_, deps, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example"))
	if err == nil || !strings.Contains(err.Error(), "IUO_IMMICH_API_KEY_FILE") {
		t.Fatalf("error should name the legacy variable actually used, got %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("deprecations should be returned even on error: %+v", deps)
	}
}

func TestLegacyEnvUsageNoLegacy(t *testing.T) {
	lookup := func(k string) (string, bool) {
		if k == "IO_MODE" {
			return "proxy", true
		}
		if k == "IUO_MODE" {
			return "", true // empty counts as unset
		}
		return "", false
	}
	if d := legacyEnvUsage(lookup); len(d) != 0 {
		t.Fatalf("expected none, got %+v", d)
	}
	if src := envSource("mode", lookup); src != "IO_MODE" {
		t.Fatalf("envSource = %q", src)
	}
}

func TestTasksFileInWatchDirCwdIsSkipped(t *testing.T) {
	clearConfigEnv(t)
	watch := t.TempDir()
	if err := os.WriteFile(filepath.Join(watch, "tasks.yaml"), []byte("tasks: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(watch)
	// Auto-resolution must not pick (or reject) a tasks.yaml that is media in
	// the watch directory; it falls through to the bundled default.
	stat := func(p string) (os.FileInfo, error) {
		if p == defaultTasksFile {
			return os.Stat(filepath.Join(watch, "tasks.yaml"))
		}
		return os.Stat(p)
	}
	got, err := resolveTasksFile("", watch, stat)
	if err != nil || got != defaultTasksFile {
		t.Fatalf("expected bundled default, got %q %v", got, err)
	}
	// Through parseConfig in watcher mode this must not be a fatal error.
	args := []string{"-mode", "watcher", "-immich_url", "http://immich.example", "-immich_api_key", "0123456789abcdef",
		"-watch_dir", watch, "-undone_dir", filepath.Join(t.TempDir(), "undone")}
	_, _, err = parseConfig(args)
	if err != nil && strings.Contains(err.Error(), "must not be inside the watch directory") {
		t.Fatalf("auto-detected tasks.yaml in the watch dir must be skipped: %v", err)
	}
}

func TestDeprecationWarningShownAtErrorLevel(t *testing.T) {
	var buf strings.Builder
	base := log.New(&buf, "", 0)
	quiet := customlogger.NewWithLevel(base, "error", "")
	quiet.Warnf("filtered")
	warnDeprecations(base, []deprecation{{Old: "IUO_UPSTREAM", New: "IO_IMMICH_URL"}})
	out := buf.String()
	if strings.Contains(out, "filtered") {
		t.Fatalf("sanity: warn level should be filtered at error level")
	}
	if !strings.Contains(out, "[WARN]") || !strings.Contains(out, "IUO_UPSTREAM -> IO_IMMICH_URL") {
		t.Fatalf("deprecation warning must be printed regardless of log level, got %q", out)
	}
}

func TestLoadTasksMotionPhotosPolicy(t *testing.T) {
	task := func(extra string) string {
		return "  - name: t\n    command: \"\"\n    extensions: [jpg]\n" + extra
	}
	cases := []struct {
		yaml                 string
		wantGlobal, wantTask string
		wantErr              bool
	}{
		// Built-in default: keep as is.
		{"tasks:\n" + task(""), entity.MotionPhotosKeep, entity.MotionPhotosKeep, false},
		// The top-level value is the default for every task...
		{"motion_photos: drop_video\ntasks:\n" + task(""), entity.MotionPhotosDropVideo, entity.MotionPhotosDropVideo, false},
		// ...and a task can override it.
		{"motion_photos: drop_video\ntasks:\n" + task("    motion_photos: drop_photo\n"), entity.MotionPhotosDropVideo, entity.MotionPhotosDropPhoto, false},
		{"tasks:\n" + task("    motion_photos: drop_video\n"), entity.MotionPhotosKeep, entity.MotionPhotosDropVideo, false},
		{"motion_photos: sometimes\ntasks:\n" + task(""), "", "", true},
		{"tasks:\n" + task("    motion_photos: sometimes\n"), "", "", true},
	}
	for _, c := range cases {
		p := filepath.Join(t.TempDir(), "tasks.yaml")
		if err := os.WriteFile(p, []byte(c.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := loadTasks(p)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error", c.yaml)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.yaml, err)
			continue
		}
		if cfg.MotionPhotos != c.wantGlobal || cfg.Tasks[0].MotionPhotos != c.wantTask {
			t.Errorf("%q: global=%q task=%q, want %q / %q", c.yaml, cfg.MotionPhotos, cfg.Tasks[0].MotionPhotos, c.wantGlobal, c.wantTask)
		}
	}
}

// Every format Immich accepts (server/src/utils/mime-types.ts, v3.4.2) must
// be handled by every bundled profile; an unmatched upload is rejected.
func TestBundledProfilesCoverImmichFormats(t *testing.T) {
	immich := strings.Fields(`
		avif bmp gif jfif jpeg jpg png webp heic heif hif insp jp2 jpe jxl mpo svg tif tiff
		3fr ari arw cap cin cr2 cr3 crw dcr dng erf fff iiq k25 kdc mrw nef nrw orf ori pef psd
		raf raw rw2 rwl sr2 srf srw x3f
		3gp 3gpp avi flv insv m2t m2ts m4v mkv mov mp4 mpe mpeg mpg mts mxf ts vob webm wmv`)
	profiles, err := filepath.Glob("../../config/*/tasks.yaml")
	if err != nil || len(profiles) < 4 {
		t.Fatalf("bundled profiles not found: %v %v", profiles, err)
	}
	for _, p := range profiles {
		cfg, err := loadTasks(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		have := map[string]bool{}
		for _, task := range cfg.Tasks {
			for _, e := range task.Extensions {
				have[e] = true
			}
		}
		for _, e := range immich {
			if !have[e] {
				t.Errorf("%s: no task handles .%s", p, e)
			}
		}
	}
}

func TestLoadTasksRejectsLegacyFolderPlaceholder(t *testing.T) {
	for _, cmd := range []string{
		`cjxl {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}.jxl`,
		`cjxl {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.nmae}}.jxl`,
	} {
		p := filepath.Join(t.TempDir(), "tasks.yaml")
		yaml := "tasks:\n  - name: t\n    command: '" + cmd + "'\n    extensions: [jpg]\n"
		if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadTasks(p); err == nil {
			t.Errorf("expected an error for command %q", cmd)
		}
	}
}

func TestLibraryModeConfig(t *testing.T) {
	base := []string{"-mode", "library", "-immich_url", "http://immich:2283", "-undone_dir", t.TempDir(), "-tasks_file", "../../config/lossless/tasks.yaml"}

	// Dry run is the default: a first start must never change a library.
	cfg, _, err := parseConfig(append(base, "-immich_api_key", "0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LibraryDryRun || cfg.LibraryLimit != 0 || cfg.LibraryReadyTimeoutSeconds != 600 {
		t.Errorf("library defaults: %+v", cfg)
	}

	if _, _, err := parseConfig(base); err == nil {
		t.Error("library mode without an API key must be refused")
	}
	if _, _, err := parseConfig(append(base, "-immich_api_key", "0123456789abcdef", "-library_taken_before", "last year")); err == nil {
		t.Error("a bad date must be refused")
	}
	if _, _, err := parseConfig(append(base, "-immich_api_key", "0123456789abcdef", "-library_limit", "-1")); err == nil {
		t.Error("a negative limit must be refused")
	}
	if d, err := parseLibraryDate("2022-12-31"); err != nil || d.Year() != 2022 {
		t.Errorf("parseLibraryDate: %v %v", d, err)
	}
	if !hasMode("watcher, Library", "library") || hasMode("watcher,proxy", "library") {
		t.Error("hasMode")
	}
}

func TestBundledLosslessProfileHasVerifyCommands(t *testing.T) {
	cfg, err := loadTasks("../../config/lossless/tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	verified := map[string]bool{}
	for _, task := range cfg.Tasks {
		if task.Verify != "" {
			for _, e := range task.Extensions {
				verified[e] = true
			}
		}
	}
	for _, e := range []string{"jpg", "jpeg", "png"} {
		if !verified[e] {
			t.Errorf("lossless profile has no verify command for .%s", e)
		}
	}
}
