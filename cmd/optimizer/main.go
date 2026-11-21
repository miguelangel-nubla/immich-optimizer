package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/spf13/viper"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/filesystem"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/httpclient"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/immich"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/mediacheck"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/motionphoto"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/processor"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/proxy"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/undone"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/watcher"
	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/usecase/convert"
	"github.com/miguelangel-nubla/immich-optimizer/internal/usecase/library"
	"github.com/miguelangel-nubla/immich-optimizer/internal/usecase/pipeline"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// flagOutput is where usage and flag errors are printed (overridable in tests).
var flagOutput io.Writer = os.Stderr

const defaultTasksFile = "/etc/immich-optimizer/config/tasks.yaml"

func parseConfig(args []string) (*entity.AppConfig, error) {
	ac := &entity.AppConfig{}

	v := viper.New()
	if err := bindEnv(v); err != nil {
		return nil, err
	}

	fs := flag.NewFlagSet("immich-optimizer", flag.ContinueOnError)
	fs.SetOutput(flagOutput)
	fs.BoolVar(&ac.ShowVersion, "version", false, "Show the current version")
	fs.BoolVar(&ac.HealthCheck, "healthcheck", false, "Check that a running optimizer answers, and exit")
	envErr := registerSettings(fs, v, ac)
	fs.Usage = func() { printUsage(fs) }

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if ac.ShowVersion {
		return ac, nil
	}
	if envErr != nil {
		return nil, envErr
	}

	if ac.HealthCheck {
		return ac, nil
	}

	visited := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { visited[f.Name] = true })

	if ac.ImmichAPIKeyFile != "" {
		source := envSource("immich_api_key_file", osLookup)
		if visited["immich_api_key_file"] || source == "" {
			source = "-immich_api_key_file"
		}
		key, err := readSecretFile(source, ac.ImmichAPIKeyFile)
		if err != nil {
			return nil, err
		}
		ac.ImmichAPIKey = key
	}
	ac.ImmichAPIKeys = splitKeys(ac.ImmichAPIKey)
	if len(ac.ImmichAPIKeys) > 0 {
		ac.ImmichAPIKey = ac.ImmichAPIKeys[0]
	}

	if err := validateAppConfig(ac); err != nil {
		return nil, err
	}

	return ac, nil
}

// printUsage prints the settings by group.
func printUsage(fs *flag.FlagSet) {
	out := fs.Output()
	fmt.Fprintf(out, "Usage of %s:\n", fs.Name())
	fmt.Fprintln(out, "Every setting is a flag and an environment variable (in brackets); a flag wins.")
	for _, g := range settingGroups {
		fmt.Fprintf(out, "\n%s:\n", g)
		for _, st := range settings {
			if st.Group != g {
				continue
			}
			line := fmt.Sprintf("  -%s  [%s]\n    \t%s", st.Name, st.Env(), st.plainHelp())
			switch d := st.Default.(type) {
			case time.Duration:
				line += fmt.Sprintf(" (a duration such as 90s or 15m; default %s)", shortDuration(d))
			case string:
				if d != "" {
					line += fmt.Sprintf(" (default %q)", d)
				}
			default:
				line += fmt.Sprintf(" (default %v)", d)
			}
			fmt.Fprintln(out, line)
		}
	}
	fmt.Fprintln(out, "\n  -version\n    \tShow the current version")
	fmt.Fprintln(out, "  -healthcheck\n    \tCheck that the optimizer running with these settings answers, and exit (used by the Docker image)")
}

// resolveTasksFile returns the explicit path, or the first existing file of
// ./tasks.yaml and the image's bundled default. Auto-detected candidates
// inside watchDir are skipped: in the container the working directory is the
// watch directory, and a tasks.yaml dropped there is user media to ingest,
// not configuration.
func resolveTasksFile(explicit, watchDir string, stat func(string) (os.FileInfo, error)) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	candidates := []string{"tasks.yaml", defaultTasksFile}
	for _, c := range candidates {
		if watchDir != "" && isWithin(watchDir, c) {
			continue
		}
		if st, err := stat(c); err == nil && !st.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("no tasks file found: set IO_TASKS_FILE or -tasks_file (looked for %s)", strings.Join(candidates, " and "))
}

// isWithin reports whether child is parent or inside it.
func isWithin(parent, child string) bool {
	p, err1 := filepath.Abs(parent)
	c, err2 := filepath.Abs(child)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// readSecretFile reads a secret value from path (Docker secrets style),
// stripping a single trailing newline. Errors name the setting and the path,
// never the file contents.
// splitKeys reads one or more API keys: separated by commas, or one per line
// (blank lines and lines starting with # are ignored).
func splitKeys(v string) []string {
	var keys []string
	for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			keys = append(keys, line)
		}
	}
	return keys
}

func readSecretFile(name, path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is operator-supplied configuration
	if err != nil {
		return "", fmt.Errorf("%s: unable to read secret file %q: %w", name, path, err)
	}
	value := strings.TrimSuffix(string(data), "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" {
		return "", fmt.Errorf("%s: secret file %q is empty", name, path)
	}
	return value, nil
}

func parseModes(modeStr string) (runWatcher bool, runProxy bool, err error) {
	rawModes := strings.Split(strings.ToLower(modeStr), ",")
	var recognizedCount int

	for _, m := range rawModes {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		switch m {
		case "watcher":
			runWatcher = true
			recognizedCount++
		case "proxy":
			runProxy = true
			recognizedCount++
		case "library":
			// Reported by hasMode: it does not change the two flags above.
			recognizedCount++
		default:
			return false, false, fmt.Errorf("unrecognized operating mode %q (supported modes: watcher, proxy, library)", m)
		}
	}

	if recognizedCount == 0 {
		return false, false, fmt.Errorf("no mode is set: set IO_MODE (or -mode) to proxy, watcher or library, or to several separated by commas")
	}

	return runWatcher, runProxy, nil
}

// hasMode reports whether the comma-separated mode list contains mode.
func hasMode(modeStr, mode string) bool {
	for _, m := range strings.Split(strings.ToLower(modeStr), ",") {
		if strings.TrimSpace(m) == mode {
			return true
		}
	}
	return false
}

func validateAppConfig(ac *entity.AppConfig) error {
	runWatcher, _, err := parseModes(ac.Mode)
	if err != nil {
		return err
	}

	if ac.ImmichURL == "" {
		return fmt.Errorf("the -immich_url flag is required")
	}

	if ac.MaxConcurrentRequests <= 0 {
		return fmt.Errorf("max_concurrent_requests must be greater than 0")
	}
	if ac.ImmichTimeout <= 0 {
		return fmt.Errorf("immich_timeout must be greater than 0")
	}
	if ac.InotifyBufferSize <= 0 {
		return fmt.Errorf("inotify_buffer_size must be greater than 0")
	}
	if ac.TasksTimeout < 0 {
		return fmt.Errorf("tasks_timeout must be 0 (no limit) or greater")
	}
	if ac.ShutdownTimeout < 0 {
		return fmt.Errorf("shutdown_timeout must be 0 or greater")
	}
	if ac.UndoneDir == "" {
		return fmt.Errorf("undone_dir must not be empty")
	}

	parsedURL, urlErr := url.Parse(ac.ImmichURL)
	if urlErr != nil {
		return fmt.Errorf("invalid immich_url format: %w", urlErr)
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return fmt.Errorf("immich_url must use http or https scheme")
	}
	if parsedURL.Host == "" {
		return fmt.Errorf("immich_url must include a valid host")
	}

	if hasMode(ac.Mode, "library") {
		if len(ac.ImmichAPIKeys) == 0 {
			return fmt.Errorf("library mode needs an Immich API key (-immich_api_key)")
		}
		seen := map[string]bool{}
		for i, k := range ac.ImmichAPIKeys {
			if len(k) < 10 {
				return fmt.Errorf("immich_api_key: key %d appears to be too short", i+1)
			}
			if seen[k] {
				return fmt.Errorf("immich_api_key: key %d is given twice", i+1)
			}
			seen[k] = true
		}
		if ac.LibraryConcurrency < 1 {
			return fmt.Errorf("library_concurrency must be 1 or more")
		}
	}

	if runWatcher {
		if ac.ImmichAPIKey == "" {
			return fmt.Errorf("the -immich_api_key flag is required in watcher mode")
		}

		if len(strings.TrimSpace(ac.ImmichAPIKey)) < 10 {
			return fmt.Errorf("immich_api_key appears to be too short")
		}

		if isWithin(ac.WatchDir, ac.UndoneDir) {
			return fmt.Errorf("undone_dir %q must not be the watch directory or inside it (%q): failed files would be re-ingested", ac.UndoneDir, ac.WatchDir)
		}

		if mkdirErr := os.MkdirAll(ac.WatchDir, 0750); mkdirErr != nil {
			return fmt.Errorf("error creating watch directory: %v", mkdirErr)
		}
	}

	// The undone directory belongs to the conversion engine: every mode uses it.
	if err := undone.CheckWritable(ac.UndoneDir); err != nil {
		return err
	}

	ac.ConfigFile, err = resolveTasksFile(ac.ConfigFile, ac.WatchDir, os.Stat)
	if err != nil {
		return err
	}
	if runWatcher && isWithin(ac.WatchDir, ac.ConfigFile) {
		return fmt.Errorf("tasks file %q must not be inside the watch directory %q: it would be ingested", ac.ConfigFile, ac.WatchDir)
	}

	ac.Tasks, err = loadTasks(ac.ConfigFile)
	if err != nil {
		return fmt.Errorf("error loading config file: %v", err)
	}

	return nil
}

func loadTasks(configFile string) (*entity.Config, error) {
	var c *entity.Config
	v := viper.New()
	v.SetConfigFile(configFile)

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}
	if c == nil {
		c = &entity.Config{}
	}

	if c.MotionPhotos == "" {
		c.MotionPhotos = entity.MotionPhotosKeep
	}
	if !entity.ValidMotionPhotos(c.MotionPhotos) {
		return nil, fmt.Errorf("error validating config: %s", motionPhotosError("top-level", c.MotionPhotos))
	}

	for i := range c.Tasks {
		// A task without its own setting uses the top-level default.
		if c.Tasks[i].MotionPhotos == "" {
			c.Tasks[i].MotionPhotos = c.MotionPhotos
		}
		if !entity.ValidMotionPhotos(c.Tasks[i].MotionPhotos) {
			return nil, fmt.Errorf("error validating config: %s", motionPhotosError("task "+c.Tasks[i].Name, c.Tasks[i].MotionPhotos))
		}
		if err := initTask(&c.Tasks[i]); err != nil {
			return nil, fmt.Errorf("error validating config: %w", err)
		}
	}

	for i := range c.Downloads {
		// A download is converted as it is stored: nothing is split off.
		c.Downloads[i].MotionPhotos = entity.MotionPhotosKeep
		if c.Downloads[i].Command == "" {
			return nil, fmt.Errorf("error validating config: download task %s has no command", c.Downloads[i].Name)
		}
		if err := initTask(&c.Downloads[i]); err != nil {
			return nil, fmt.Errorf("error validating config: download %w", err)
		}
	}

	return c, nil
}

// configFingerprint identifies the content of the folder the tasks file is
// in: the file itself and whatever its commands call from there (scripts,
// presets). It goes into every marker, so that conversions made with one
// configuration can be told from those made with another, also when a task
// kept its name. It is "" when the folder cannot be read or is unreasonably
// big; a marker then simply has none.
func configFingerprint(dir string, logger *customlogger.Logger) string {
	const maxFiles, maxBytes = 2000, 64 << 20
	type entry struct {
		rel string
		sum [sha256.Size]byte
	}
	var entries []entry
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Mounted volumes carry their own bookkeeping folders.
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		if len(entries) >= maxFiles || total > maxBytes {
			return errors.New("too big to fingerprint")
		}
		data, err := os.ReadFile(path) // #nosec G304,G122 -- the operator's own configuration folder
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		entries = append(entries, entry{filepath.ToSlash(rel), sha256.Sum256(data)})
		return nil
	})
	if err != nil {
		logger.Warnf("Markers will carry no configuration fingerprint: %s: %v", dir, err)
		return ""
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%d:%s", len(e.rel), e.rel)
		h.Write(e.sum[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func motionPhotosError(where, got string) string {
	return fmt.Sprintf("%s motion_photos must be %q, %q or %q, got %q", where,
		entity.MotionPhotosKeep, entity.MotionPhotosDropVideo, entity.MotionPhotosDropPhoto, got)
}

func initTask(task *entity.Task) error {
	values := map[string]string{
		"src_folder": "/src_folder",
		"dst_folder": "/dst_folder",
		"name":       "name",
		"extension":  "ext",
	}

	for i, ext := range task.Extensions {
		task.Extensions[i] = utils.NormalizeExtension(ext)
	}

	// An unknown placeholder is an error instead of the text "<no value>"
	// ending up in the command.
	tmpl, err := template.New("command").Option("missingkey=error").Parse(task.Command)
	if err != nil {
		return fmt.Errorf("task %s unable to parse command: %v", task.Name, err)
	}

	var cmdLine bytes.Buffer
	if err := tmpl.Execute(&cmdLine, values); err != nil {
		return fmt.Errorf("task %s unable to execute template for command: %v", task.Name, err)
	}

	return nil
}

func run(args []string) error {
	config, err := parseConfig(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if config.ShowVersion {
		fmt.Println(printVersion())
		return nil
	}

	if config.HealthCheck {
		return healthCheck(config)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	baseLogger := log.New(os.Stdout, "", log.Ldate|log.Ltime)
	logger := customlogger.NewWithLevel(baseLogger, config.LogLevel, "")
	logger.Printf("Starting %s (mode: %s)", printVersion(), config.Mode)

	parsedImmichURL, urlErr := url.Parse(config.ImmichURL)
	if urlErr != nil {
		return fmt.Errorf("invalid immich_url: %w", urlErr)
	}

	undoneStore, err := undone.NewLocalStore(config.UndoneDir, version, logger)
	if err != nil {
		return err
	}
	logger.Printf("Failed inputs are saved to %s", undoneStore.Root())

	// One persistent record of every asset operation, whatever the mode.
	auditFile := config.AuditLogFile
	if auditFile == "" {
		auditFile = filepath.Join(config.UndoneDir, "audit.log")
	}
	auditLog, err := audit.Open(auditFile)
	if err != nil {
		return err
	}
	defer auditLog.Close()
	// Mandatory: prove it can be written before anything else starts.
	if err := auditLog.Write(audit.Entry{Source: "optimizer", Event: "started", Detail: printVersion() + ", mode " + config.Mode}); err != nil {
		return fmt.Errorf("the audit log %s cannot be written, refusing to start: %w", auditFile, err)
	}
	logger.Printf("Asset operations are recorded in %s", auditFile)

	processorAdapter := processor.NewLocalProcessor(logger, filepath.Dir(config.ConfigFile))
	configSHA256 := configFingerprint(filepath.Dir(config.ConfigFile), logger)
	engine := convert.New(convert.Config{
		ConfigSHA256:  configSHA256,
		Tasks:         config.Tasks.Tasks,
		MaxConcurrent: config.MaxConcurrentRequests,
		TaskTimeout:   config.TasksTimeout,
		SaveUnmatched: config.UndoneUnmatched,
		Version:       version,
		Audit:         auditLog,
		Checker:       mediacheck.New(),
	}, processorAdapter, undoneStore, logger)

	httpTimeout := config.ImmichTimeout
	httpClient := httpclient.New(httpclient.Options{
		DialTimeout:           httpTimeout,
		TLSHandshakeTimeout:   httpclient.DefaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: httpTimeout,
		StallTimeout:          httpTimeout,
	})

	var watcherAdapter port.FileWatcher
	var pipelineService *pipeline.Service
	var proxyServer *proxy.Server

	runWatcher, runProxy, _ := parseModes(config.Mode)

	if runWatcher {
		immichAdapter := immich.NewClient(config.ImmichURL, config.ImmichAPIKey, httpClient, logger)

		var err error
		watcherAdapter, err = watcher.NewInotifyWatcher(config.WatchDir, logger, config.InotifyBufferSize)
		if err != nil {
			return fmt.Errorf("error creating file watcher: %w", err)
		}

		pipelineService = pipeline.NewService(
			watcherAdapter,
			engine,
			immichAdapter,
			filesystem.NewLocalFileSystem(),
			logger,
			config.MaxConcurrentRequests,
			config.WatchDir,
			auditLog,
		)

		if err := watcherAdapter.Start(); err != nil {
			return fmt.Errorf("error starting watcher: %w", err)
		}
		pipelineService.Start()
	}

	if runProxy {
		proxyServer = proxy.NewServer(proxy.Config{
			UpstreamURL: parsedImmichURL,
			BindAddr:    config.BindAddr,
			PassUploads: !config.ProxyConvertUploads,
			Downloads:   config.Tasks.Downloads,
			Processor:   processorAdapter,
			// The same limits as for uploads.
			DownloadTimeout:     config.TasksTimeout,
			DownloadConcurrency: config.MaxConcurrentRequests,
			HTTPClient:          httpClient,
			Audit:               auditLog,
		}, engine, logger)
		if err := proxyServer.Start(); err != nil {
			if watcherAdapter != nil {
				_ = watcherAdapter.Stop()
			}
			return fmt.Errorf("error starting proxy server: %w", err)
		}
	}

	// Library mode runs until the process stops: it reacts to tags in
	// Immich. It only ends by itself on an error it cannot go on after.
	var libraryDone chan error
	libraryCtx, cancelLibrary := context.WithCancel(context.Background())
	defer cancelLibrary()
	if hasMode(config.Mode, "library") {
		// One worker per key: Immich lets a key act on its own user's
		// assets only. They share the conversion engine and the audit log.
		keys := config.ImmichAPIKeys
		libraryDone = make(chan error, len(keys))
		for i, key := range keys {
			label := ""
			if len(keys) > 1 {
				label = fmt.Sprintf("key %d of %d", i+1, len(keys))
			}
			svc := library.New(library.Config{
				Label:         label,
				ReadyTimeout:  config.LibraryReadyTimeout,
				Version:       version,
				ConfigSHA256:  configSHA256,
				Concurrency:   config.LibraryConcurrency,
				IsMotionPhoto: motionphoto.IsMotionJPEG,
			}, immich.NewLibraryClient(config.ImmichURL, key, httpClient), engine, auditLog, logger)
			events := immich.NewEventClient(config.ImmichURL, key)
			events.Errorf = svc.Warnf
			go func() { libraryDone <- svc.Serve(libraryCtx, events) }()
		}
		logger.Printf("Library mode: converting what is tagged %s in Immich, for %d user(s); originals go to Immich's trash", library.TagConvert, len(keys))
	}

	// Wait for interrupt, or for library mode to fail.
	running := cap(libraryDone)
	select {
	case <-sigChan:
	case err := <-libraryDone:
		running--
		if err != nil {
			if !runWatcher && !runProxy {
				cancelLibrary()
				return fmt.Errorf("library mode: %w", err)
			}
			logger.Errorf("Library mode stopped: %v", err)
		}
		<-sigChan
	}
	cancelLibrary()
	// Let the assets in flight be rolled back cleanly. If this times out the
	// next start finds the unfinished copies and resolves them.
	timeout := time.After(config.ShutdownTimeout)
	for ; running > 0; running-- {
		select {
		case err := <-libraryDone:
			if err != nil {
				logger.Errorf("Library mode stopped: %v", err)
			}
		case <-timeout:
			logger.Warnf("Library mode did not stop in time; the next start will resolve what was in progress")
			running = 0
		}
	}

	grace := config.ShutdownTimeout
	logger.Printf("Shutting down gracefully (waiting up to %s for running jobs)...", grace)
	shutdown(logger, grace, watcherAdapter, pipelineService, proxyServer)
	return nil
}

// healthCheck is what the Docker image's HEALTHCHECK runs, in a second
// process next to the optimizer. In proxy mode it asks the proxy itself (not
// Immich: an Immich that is down is not a reason to restart the optimizer).
// The other modes have nothing to ask: that this ran at all is the answer.
func healthCheck(config *entity.AppConfig) error {
	_, runProxy, err := parseModes(config.Mode)
	if err != nil || !runProxy {
		return err
	}
	host, port, err := net.SplitHostPort(config.BindAddr)
	if err != nil {
		return fmt.Errorf("proxy_bind_addr %q: %w", config.BindAddr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + proxy.HealthPath)
	if err != nil {
		return fmt.Errorf("the proxy does not answer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the proxy answered %d", resp.StatusCode)
	}
	return nil
}

// drainer is implemented by sources that can finish running work on shutdown.
type drainer interface {
	Drain(ctx context.Context) error
	Cancel()
}

// shutdown stops intake, lets running jobs finish within grace, then cancels
// them (killing task process groups) and waits a little longer.
func shutdown(logger *customlogger.Logger, grace time.Duration, w port.FileWatcher, p *pipeline.Service, ps *proxy.Server) {
	if w != nil {
		_ = w.Stop()
	}
	if ps != nil {
		ps.BeginShutdown()
	}

	var sources []drainer
	if p != nil {
		sources = append(sources, p)
	}
	if ps != nil {
		sources = append(sources, ps)
	}

	drainAll := func(d time.Duration) bool {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		var wg sync.WaitGroup
		ok := true
		var mu sync.Mutex
		for _, s := range sources {
			wg.Add(1)
			go func(s drainer) {
				defer wg.Done()
				if err := s.Drain(ctx); err != nil {
					mu.Lock()
					ok = false
					mu.Unlock()
				}
			}(s)
		}
		wg.Wait()
		return ok
	}

	if drainAll(grace) {
		logger.Printf("Shutdown completed successfully")
	} else {
		logger.Warnf("Shutdown grace period (%s) expired; cancelling running jobs (they will be retried: watcher files stay in place, proxy clients get 503)", grace)
		for _, s := range sources {
			s.Cancel()
		}
		if !drainAll(2 * time.Second) {
			logger.Printf("Shutdown timeout exceeded, forcing exit")
		}
	}
	if ps != nil {
		// Let handlers write the results of jobs that just finished (or the
		// 503 of cancelled ones) before connections are force-closed.
		ctx, cancel := context.WithTimeout(context.Background(), handlerFlushTimeout)
		defer cancel()
		if err := ps.FinishShutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("Proxy connections force-closed: %v", err)
		}
	}
}

// handlerFlushTimeout bounds how long shutdown waits for proxy handlers to
// write finished results after all jobs are done.
const handlerFlushTimeout = 3 * time.Second

func printVersion() string {
	return fmt.Sprintf("immich-optimizer %s, commit %s, built at %s", version, commit, date)
}
