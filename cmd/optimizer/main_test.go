package main

import (
	"context"
	"errors"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/proxy"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

// clearConfigEnv blanks every IO_* variable so the developer's
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

	cfg, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-tasks_concurrency", "3",
		"-immich_timeout", "45s",
		"-watcher_inotify_buffer_size", "4096",
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
	if cfg.ImmichTimeout != 45*time.Second {
		t.Fatalf("expected HTTP timeout 45, got %s", cfg.ImmichTimeout)
	}
	if cfg.InotifyBufferSize != 4096 {
		t.Fatalf("expected inotify buffer size 4096, got %d", cfg.InotifyBufferSize)
	}
}

func TestParseConfigRejectsInvalidMaxConcurrentRequests(t *testing.T) {
	clearConfigEnv(t)
	tasksFile := writeTestTasksFile(t)

	_, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-tasks_concurrency", "0",
	})
	if err == nil {
		t.Fatalf("expected invalid max_concurrent_requests to fail")
	}
}

func TestParseConfigRejectsInvalidHTTPTimeoutSeconds(t *testing.T) {
	clearConfigEnv(t)
	tasksFile := writeTestTasksFile(t)

	_, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-immich_timeout", "0",
	})
	if err == nil {
		t.Fatalf("expected invalid http_timeout_seconds to fail")
	}
}

func TestParseConfigRejectsInvalidInotifyBufferSize(t *testing.T) {
	clearConfigEnv(t)
	tasksFile := writeTestTasksFile(t)

	_, err := parseConfig([]string{
		"-mode", "proxy",
		"-immich_url", "http://immich.example",
		"-tasks_file", tasksFile,
		"-watcher_inotify_buffer_size", "0",
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
	cfg, err := parseConfig(args)
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

// ---- IO_ prefix ----

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
		"IO_TASKS_CONCURRENCY":           "4",
		"IO_IMMICH_TIMEOUT":              "33s",
		"IO_TASKS_TIMEOUT":               "10m",
		"IO_SHUTDOWN_TIMEOUT":            "20s",
		"IO_UNDONE_DIR":                  undoneDir,
		"IO_UNDONE_UNMATCHED":            "false",
		"IO_PROXY_BIND_ADDR":             ":9999",
		"IO_WATCHER_DIR":                 watchDir,
		"IO_WATCHER_INOTIFY_BUFFER_SIZE": "1024",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
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
		{"http", int(cfg.ImmichTimeout.Seconds()), 33},
		{"task timeout", int(cfg.TasksTimeout.Seconds()), 600},
		{"shutdown", int(cfg.ShutdownTimeout.Seconds()), 20},
		{"undone", cfg.UndoneDir, undoneDir},
		{"undone unmatched", cfg.UndoneUnmatched, false},
		{"bind", cfg.BindAddr, ":9999"},
		{"watch", cfg.WatchDir, watchDir},
		{"inotify", cfg.InotifyBufferSize, 1024},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

// The names of immich-upload-optimizer are gone: an old flag is unknown and an
// old variable is not read.
func TestOldNamesNotAccepted(t *testing.T) {
	clearConfigEnv(t)
	flagOutput = io.Discard
	defer func() { flagOutput = os.Stderr }()
	if _, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-upstream", "http://immich.example")); err == nil || !strings.Contains(err.Error(), "upstream") {
		t.Fatalf("-upstream should be an unknown flag, got %v", err)
	}
	t.Setenv("IUO_IMMICH_URL", "http://immich.example")
	if _, err := parseConfig(append(baseArgs(t), "-mode", "proxy")); err == nil || !strings.Contains(err.Error(), "immich_url") {
		t.Fatalf("IUO_IMMICH_URL should be ignored, got %v", err)
	}
}

func TestDefaultBindAddr2283(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example"))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.BindAddr != ":2283" {
		t.Fatalf("default bind addr = %q, want :2283", cfg.BindAddr)
	}
	if !cfg.UndoneUnmatched || cfg.TasksTimeout != 0 || cfg.ShutdownTimeout != 8*time.Second {
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
	_, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example", "-undone_dir", dir))
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
	_, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example", "-undone_dir", ro))
	if err == nil || !strings.Contains(err.Error(), "not writable by uid") {
		t.Fatalf("expected not-writable error, got %v", err)
	}
}

func TestUndoneInsideWatchRejected(t *testing.T) {
	clearConfigEnv(t)
	watch := t.TempDir()
	for _, undoneDir := range []string{watch, filepath.Join(watch, "undone")} {
		_, err := parseConfig(append(baseArgs(t),
			"-mode", "watcher", "-immich_url", "http://immich.example", "-immich_api_key", testAPIKeyEnv,
			"-watcher_dir", watch, "-undone_dir", undoneDir))
		if err == nil || !strings.Contains(err.Error(), "inside") {
			t.Fatalf("undone %q inside watch %q should be rejected, got %v", undoneDir, watch, err)
		}
	}
	// A sibling whose name merely starts with the watch dir name is fine.
	_, err := parseConfig(append(baseArgs(t),
		"-mode", "watcher", "-immich_url", "http://immich.example", "-immich_api_key", testAPIKeyEnv,
		"-watcher_dir", watch, "-undone_dir", watch+"-undone"))
	if err != nil {
		t.Fatalf("sibling undone dir rejected: %v", err)
	}
	os.RemoveAll(watch + "-undone")
}

func TestNegativeTaskTimeoutRejected(t *testing.T) {
	clearConfigEnv(t)
	for _, flagName := range []string{"-tasks_timeout", "-shutdown_timeout"} {
		_, err := parseConfig(append(baseArgs(t), "-mode", "proxy", "-immich_url", "http://immich.example", flagName, "-1"))
		if err == nil {
			t.Fatalf("%s=-1 should be rejected", flagName)
		}
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
		"-watcher_dir", watch, "-undone_dir", filepath.Join(t.TempDir(), "undone")}
	_, err = parseConfig(args)
	if err != nil && strings.Contains(err.Error(), "must not be inside the watch directory") {
		t.Fatalf("auto-detected tasks.yaml in the watch dir must be skipped: %v", err)
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
	if err != nil || len(profiles) < 3 {
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

func TestLoadTasksRejectsUnknownPlaceholder(t *testing.T) {
	for _, cmd := range []string{
		`cjxl {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}.jxl`,
		`cjxl {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.nmae}}.jxl`,
	} {
		p := filepath.Join(t.TempDir(), "tasks.yaml")
		yaml := "tasks:\n  - name: t\n    command: '" + cmd + "'\n    extensions: [jpg]\n"
		if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadTasks(p)
		if err == nil || !strings.Contains(err.Error(), "template") {
			t.Errorf("expected a template error for command %q, got %v", cmd, err)
		}
	}
}

func TestLibraryModeConfig(t *testing.T) {
	base := []string{"-mode", "library", "-immich_url", "http://immich:2283", "-undone_dir", t.TempDir(), "-tasks_file", "../../config/lossless/tasks.yaml"}

	// Dry run is the default: a first start must never change a library.
	cfg, err := parseConfig(append(base, "-immich_api_key", "0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LibraryDryRun || cfg.LibraryReadyTimeout != 10*time.Minute {
		t.Errorf("library defaults: %+v", cfg)
	}

	if _, err := parseConfig(base); err == nil {
		t.Error("library mode without an API key must be refused")
	}
	if !hasMode("watcher, Library", "library") || hasMode("watcher,proxy", "library") {
		t.Error("hasMode")
	}
}

// docs/configuration.md carries the settings reference generated from the
// settings table. Run with IO_UPDATE_DOCS=1 to rewrite it.
func TestSettingsDocs(t *testing.T) {
	const path = "../../docs/configuration.md"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	replace := func(begin, end, content string) {
		i := strings.Index(doc, begin)
		j := strings.Index(doc, end)
		if i < 0 || j < i {
			t.Fatalf("markers %q ... %q not found in %s", begin, end, path)
		}
		i += strings.Index(doc[i:], "\n") + 1
		doc = doc[:i] + "\n" + content + doc[j:]
	}
	replace("<!-- BEGIN SETTINGS", "<!-- END SETTINGS -->", settingsMarkdown())
	if doc == string(data) {
		return
	}
	if os.Getenv("IO_UPDATE_DOCS") == "" {
		t.Fatalf("%s is out of date: run IO_UPDATE_DOCS=1 go test ./cmd/optimizer -run TestSettingsDocs", path)
	}
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The naming rule: a setting's name starts with the thing it belongs to, and
// flag and environment variable are the same name.
func TestSettingsNaming(t *testing.T) {
	prefix := map[string][]string{
		groupImmich: {"immich_"}, groupTasks: {"tasks_"}, groupUndone: {"undone_", "audit_"},
		groupProxy: {"proxy_"}, groupWatcher: {"watcher_"}, groupLibrary: {"library_"},
	}
	seen := map[string]bool{}
	for _, s := range settings {
		if seen[s.Name] {
			t.Errorf("duplicate setting %s", s.Name)
		}
		seen[s.Name] = true
		if s.Env() != "IO_"+strings.ToUpper(s.Name) || s.Help == "" || s.target == nil {
			t.Errorf("setting %s is incomplete", s.Name)
		}
		ok := s.Group == groupGeneral
		for _, p := range prefix[s.Group] {
			ok = ok || strings.HasPrefix(s.Name, p)
		}
		if !ok {
			t.Errorf("setting %s does not carry the prefix of its group %q", s.Name, s.Group)
		}
	}
}

// Every relative link and anchor in the documentation must resolve, and every
// IO_* variable it mentions must exist.
func TestDocsLinksAndSettingNames(t *testing.T) {
	files, _ := filepath.Glob("../../docs/*.md")
	files = append(files, "../../README.md")
	headingRe := regexp.MustCompile(`(?m)^#+\s+(.*)$`)
	strip := regexp.MustCompile(`[^\p{L}\p{N}\- _]`)
	anchors := func(path string) map[string]bool {
		data, _ := os.ReadFile(path)
		out := map[string]bool{}
		for _, m := range headingRe.FindAllStringSubmatch(string(data), -1) {
			a := strings.ToLower(strip.ReplaceAllString(strings.TrimSpace(m[1]), ""))
			out[strings.ReplaceAll(a, " ", "-")] = true
		}
		return out
	}
	known := map[string]bool{"IO_UPDATE_DOCS": true, "IO_DEV_DIR": true,
		// Read by the test scripts.
		"IO_IOS_FULL_SPEED": true, "IO_IOS_DEVICES": true,
		// Given to task commands, not settings.
		"IO_MOTION_PHOTOS": true, "IO_SRC_FOLDER": true, "IO_DST_FOLDER": true, "IO_NAME": true, "IO_EXTENSION": true,
		"IO_ORIGINAL_NAME": true, "IO_SOURCE": true, "IO_TASK": true}
	for _, s := range settings {
		known[s.Env()] = true
	}
	linkRe := regexp.MustCompile(`\]\(([^)\s]+)\)`)
	envRe := regexp.MustCompile(`IO_[A-Z][A-Z0-9_]*[A-Z0-9]`)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range linkRe.FindAllStringSubmatch(string(data), -1) {
			link := m[1]
			if strings.Contains(link, "://") {
				continue
			}
			path, anchor, _ := strings.Cut(link, "#")
			target := f
			if path != "" {
				target = filepath.Join(filepath.Dir(f), path)
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: link %q points at a missing file", f, link)
				continue
			}
			if anchor != "" && strings.HasSuffix(target, ".md") && !anchors(target)[anchor] {
				t.Errorf("%s: link %q points at a missing heading", f, link)
			}
		}
		for _, name := range envRe.FindAllString(string(data), -1) {
			isPrefix := false // e.g. "IO_PROXY_…" in prose
			for k := range known {
				isPrefix = isPrefix || strings.HasPrefix(k, name+"_")
			}
			if !known[name] && !isPrefix {
				t.Errorf("%s mentions %s, which is not a setting", f, name)
			}
		}
	}
}

func TestDurationSettings(t *testing.T) {
	base := []string{"-mode", "proxy", "-immich_url", "http://immich:2283", "-undone_dir", t.TempDir(), "-tasks_file", "../../config/lossless/tasks.yaml"}
	lookup := osLookup
	defer func() { osLookup = lookup }()
	env := map[string]string{}
	osLookup = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	parse := func(args ...string) (*entity.AppConfig, error) {
		for k := range env {
			t.Setenv(k, env[k])
		}
		cfg, err := parseConfig(append(append([]string{}, base...), args...))
		return cfg, err
	}

	// Go's duration format, on flags and in the environment; "0" is zero.
	env["IO_TASKS_TIMEOUT"], env["IO_SHUTDOWN_TIMEOUT"] = "1h30m", "0"
	cfg, err := parse("-immich_timeout", "45s")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TasksTimeout != 90*time.Minute || cfg.ShutdownTimeout != 0 || cfg.ImmichTimeout != 45*time.Second || cfg.LibraryReadyTimeout != 10*time.Minute {
		t.Errorf("durations = %v %v %v %v", cfg.TasksTimeout, cfg.ShutdownTimeout, cfg.ImmichTimeout, cfg.LibraryReadyTimeout)
	}
	// A flag wins over the environment.
	if cfg, err = parse("-tasks_timeout", "5m"); err != nil || cfg.TasksTimeout != 5*time.Minute {
		t.Errorf("flag did not win: %v %v", cfg, err)
	}

	// A bare number is not a duration.
	if _, err := parse("-immich_timeout", "120"); err == nil {
		t.Error("a bare number was accepted on a flag")
	}
	env["IO_TASKS_TIMEOUT"] = "600"
	if _, err := parse(); err == nil || !strings.Contains(err.Error(), "IO_TASKS_TIMEOUT") {
		t.Errorf("a bare number was accepted in the environment: %v", err)
	}
	delete(env, "IO_TASKS_TIMEOUT")
	t.Setenv("IO_TASKS_TIMEOUT", "")

}

func TestRepeatLibrary(t *testing.T) {
	logger := customlogger.NewWithLevel(nil, "error", "")
	// Without an interval: one run, and its error is the result.
	n := 0
	boom := errors.New("boom")
	if err := repeatLibrary(context.Background(), 0, logger, func(context.Context) error { n++; return boom }); err != boom || n != 1 {
		t.Fatalf("once: %v after %d run(s)", err, n)
	}
	// With one: it keeps going, also after a failed run, until it is stopped.
	n = 0
	ctx, cancel := context.WithCancel(context.Background())
	err := repeatLibrary(ctx, time.Millisecond, logger, func(context.Context) error {
		n++
		if n == 3 {
			cancel()
		}
		if n == 1 {
			return boom
		}
		return nil
	})
	if n != 3 || !errors.Is(err, context.Canceled) && err != nil {
		t.Fatalf("repeated: %v after %d run(s)", err, n)
	}
}

func TestLibraryIntervalSetting(t *testing.T) {
	base := []string{"-mode", "library", "-immich_url", "http://immich:2283", "-immich_api_key", "0123456789abcdef",
		"-undone_dir", t.TempDir(), "-tasks_file", "../../config/lossless/tasks.yaml"}
	ac, err := parseConfig(append(base, "-library_interval", "24h"))
	if err != nil || ac.LibraryInterval != 24*time.Hour {
		t.Fatalf("%+v, %v", ac, err)
	}
	for _, bad := range [][]string{{"-library_interval", "-1h"}, {"-library_interval", "90"}, {"-library_taken_before", "2020-01-01"}, {"-library_older_than", "24h"}, {"-library_limit", "5"}} {
		if _, err := parseConfig(append(append([]string(nil), base...), bad...)); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

func TestHealthCheck(t *testing.T) {
	// Modes without a proxy have nothing to ask.
	if err := healthCheck(&entity.AppConfig{Mode: "watcher"}); err != nil {
		t.Errorf("watcher: %v", err)
	}
	asked := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path
		if r.URL.Query().Get("x") != "" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	// ":port" is how the bind address is usually written.
	if err := healthCheck(&entity.AppConfig{Mode: "proxy", BindAddr: ":" + port}); err != nil || asked != proxy.HealthPath {
		t.Errorf("proxy: %v, asked %q", err, asked)
	}
	srv.Close()
	if err := healthCheck(&entity.AppConfig{Mode: "watcher,proxy", BindAddr: "127.0.0.1:" + port}); err == nil {
		t.Error("a proxy that does not answer was reported healthy")
	}
	// The flag needs none of the other settings to be valid.
	if ac, err := parseConfig([]string{"-healthcheck"}); err != nil || !ac.HealthCheck {
		t.Errorf("parseConfig: %+v, %v", ac, err)
	}
}

func TestLoadTasksDownloads(t *testing.T) {
	load := func(content string) (*entity.Config, error) {
		t.Helper()
		f := filepath.Join(t.TempDir(), "tasks.yaml")
		if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return loadTasks(f)
	}
	c, err := load("downloads:\n- name: d\n  command: djxl \"{{.src_folder}}/{{.name}}.{{.extension}}\" \"{{.dst_folder}}/{{.name}}.jpg\"\n  extensions: [JXL]\ntasks: []\n")
	if err != nil || len(c.Downloads) != 1 || c.Downloads[0].Extensions[0] != "jxl" || c.Downloads[0].MotionPhotos != entity.MotionPhotosKeep {
		t.Fatalf("%+v, %v", c, err)
	}
	// A download task has to do something, and its command is checked like any other.
	if _, err := load("downloads:\n- name: d\n  command: \"\"\n  extensions: [jxl]\n"); err == nil {
		t.Error("a download task without a command was accepted")
	}
	if _, err := load("downloads:\n- name: d\n  command: \"x {{.nope}}\"\n  extensions: [jxl]\n"); err == nil {
		t.Error("a download task with an unknown placeholder was accepted")
	}
}

// The variables given to task commands share the IO_ prefix with the
// settings; none may ever be mistaken for one.
func TestCommandVariablesAreNotSettings(t *testing.T) {
	for _, name := range []string{"IO_SRC_FOLDER", "IO_DST_FOLDER", "IO_NAME", "IO_EXTENSION", "IO_ORIGINAL_NAME", "IO_SOURCE", "IO_TASK", "IO_MOTION_PHOTOS"} {
		for _, s := range settings {
			if s.Env() == name {
				t.Errorf("%s is both a setting and a task command variable", name)
			}
		}
	}
}

func TestConfigFingerprint(t *testing.T) {
	logger := customlogger.NewWithLevel(nil, "error", "")
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tasks.yaml", "tasks: []\n")
	write("convert.sh", "#!/bin/sh\ncjxl\n")
	first := configFingerprint(dir, logger)
	if len(first) != 64 || configFingerprint(dir, logger) != first {
		t.Fatalf("fingerprint %q is not stable", first)
	}
	// A script the command calls is part of what was run.
	write("convert.sh", "#!/bin/sh\ncjxl -q 80\n")
	second := configFingerprint(dir, logger)
	if second == first {
		t.Error("changing a script did not change the fingerprint")
	}
	// So is a file's name, and a file in a subfolder.
	write("presets/handbrake.json", "{}")
	if configFingerprint(dir, logger) == second {
		t.Error("a new file did not change the fingerprint")
	}
	// Bookkeeping folders of a mounted volume are not.
	third := configFingerprint(dir, logger)
	write(".git/HEAD", "ref")
	if configFingerprint(dir, logger) != third {
		t.Error("a hidden folder changed the fingerprint")
	}
	// A folder that cannot be read gives none, and does not stop anything.
	if got := configFingerprint(filepath.Join(dir, "missing"), logger); got != "" {
		t.Errorf("fingerprint of a missing folder = %q", got)
	}
}

// There is no default mode: a container started without one must say so,
// not start doing something that was not asked for.
func TestModeIsRequired(t *testing.T) {
	_, err := parseConfig(append(baseArgs(t), "-immich_url", "http://immich.example", "-immich_api_key", testAPIKeyEnv))
	if err == nil || !strings.Contains(err.Error(), "IO_MODE") {
		t.Fatalf("starting without a mode: %v", err)
	}
	if _, err := parseConfig(append(baseArgs(t), "-mode", " , ", "-immich_url", "http://immich.example")); err == nil {
		t.Error("an empty mode list was accepted")
	}
}
