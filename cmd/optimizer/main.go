package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/spf13/viper"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/filesystem"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/immich"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/processor"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/proxy"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/watcher"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
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

func parseConfig(args []string) (*entity.AppConfig, bool, error) {
	ac := &entity.AppConfig{
		MaxConcurrentRequests: 10,
		HTTPTimeoutSeconds:    120,
		InotifyBufferSize:     8192,
	}

	viper.SetEnvPrefix("iuo")
	viper.AutomaticEnv()

	_ = viper.BindEnv("mode", "IUO_MODE")
	_ = viper.BindEnv("immich_url", "IUO_IMMICH_URL")
	_ = viper.BindEnv("immich_api_key", "IUO_IMMICH_API_KEY")
	_ = viper.BindEnv("tasks_file", "IUO_TASKS_FILE")
	_ = viper.BindEnv("log_level", "IUO_LOG_LEVEL")

	_ = viper.BindEnv("bind_addr", "IUO_PROXY_BIND_ADDR", "IUO_BIND_ADDR")
	_ = viper.BindEnv("filter_path", "IUO_PROXY_FILTER_PATH", "IUO_FILTER_PATH")
	_ = viper.BindEnv("filter_form_key", "IUO_PROXY_FILTER_FORM_KEY", "IUO_FILTER_FORM_KEY")

	_ = viper.BindEnv("watch_dir", "IUO_WATCHER_WATCH_DIR", "IUO_WATCH_DIR")
	_ = viper.BindEnv("undone_dir", "IUO_WATCHER_UNDONE_DIR", "IUO_UNDONE_DIR")

	viper.SetDefault("mode", "watcher")
	viper.SetDefault("bind_addr", ":8080")
	viper.SetDefault("filter_path", "/api/assets")
	viper.SetDefault("filter_form_key", "assetData")
	viper.SetDefault("immich_url", "")
	viper.SetDefault("immich_api_key", "")
	viper.SetDefault("watch_dir", "/watch")
	viper.SetDefault("undone_dir", "/undone")
	viper.SetDefault("tasks_file", "tasks.yaml")
	viper.SetDefault("log_level", "info")

	fs := flag.NewFlagSet("immich-optimizer", flag.ContinueOnError)
	fs.BoolVar(&ac.ShowVersion, "version", false, "Show the current version")
	fs.StringVar(&ac.Mode, "mode", viper.GetString("mode"), "Operating mode: watcher or proxy (comma-separated for multiple)")
	fs.StringVar(&ac.BindAddr, "bind_addr", viper.GetString("bind_addr"), "Address for reverse proxy server to listen on")
	fs.StringVar(&ac.FilterPath, "filter_path", viper.GetString("filter_path"), "Path pattern to intercept for proxy uploads")
	fs.StringVar(&ac.FilterFormKey, "filter_form_key", viper.GetString("filter_form_key"), "Form key for upload files")
	fs.StringVar(&ac.ImmichURL, "immich_url", viper.GetString("immich_url"), "Immich server URL")
	fs.StringVar(&ac.ImmichAPIKey, "immich_api_key", viper.GetString("immich_api_key"), "Immich API key")
	fs.StringVar(&ac.WatchDir, "watch_dir", viper.GetString("watch_dir"), "Directory to watch for new files")
	fs.StringVar(&ac.UndoneDir, "undone_dir", viper.GetString("undone_dir"), "Directory to copy files that failed")
	fs.StringVar(&ac.ConfigFile, "tasks_file", viper.GetString("tasks_file"), "Path to the configuration file")
	fs.StringVar(&ac.LogLevel, "log_level", viper.GetString("log_level"), "Log level (debug, info, warn, error)")

	if err := fs.Parse(args); err != nil {
		return nil, false, err
	}

	if ac.ShowVersion {
		return ac, true, nil
	}

	if err := validateAppConfig(ac); err != nil {
		return nil, false, err
	}

	return ac, false, nil
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
		default:
			return false, false, fmt.Errorf("unrecognized operating mode %q (supported modes: watcher, proxy)", m)
		}
	}

	if recognizedCount == 0 {
		return false, false, fmt.Errorf("no valid operating mode specified")
	}

	return runWatcher, runProxy, nil
}

func validateAppConfig(ac *entity.AppConfig) error {
	runWatcher, _, err := parseModes(ac.Mode)
	if err != nil {
		return err
	}

	if ac.ImmichURL == "" {
		return fmt.Errorf("the -immich_url flag is required")
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

	if runWatcher {
		if ac.ImmichAPIKey == "" {
			return fmt.Errorf("the -immich_api_key flag is required in watcher mode")
		}

		if len(strings.TrimSpace(ac.ImmichAPIKey)) < 10 {
			return fmt.Errorf("immich_api_key appears to be too short")
		}

		if mkdirErr := os.MkdirAll(ac.WatchDir, 0750); mkdirErr != nil {
			return fmt.Errorf("error creating watch directory: %v", mkdirErr)
		}

		if mkdirErr := os.MkdirAll(ac.UndoneDir, 0750); mkdirErr != nil {
			return fmt.Errorf("error creating undone directory: %v", mkdirErr)
		}
	}

	if ac.ConfigFile == "" {
		return fmt.Errorf("the -tasks_file flag is required")
	}

	ac.Tasks, err = loadTasks(ac.ConfigFile)
	if err != nil {
		return fmt.Errorf("error loading config file: %v", err)
	}

	return nil
}

func loadTasks(configFile string) (*entity.Config, error) {
	var c *entity.Config
	viper.SetConfigFile(configFile)

	if err := viper.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	if err := viper.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("error unmarshaling config: %w", err)
	}

	for i := range c.Tasks {
		if err := initTask(&c.Tasks[i]); err != nil {
			return nil, fmt.Errorf("error validating config: %w", err)
		}
	}

	return c, nil
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

	tmpl, err := template.New("command").Parse(task.Command)
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
	config, isVersion, err := parseConfig(args)
	if err != nil {
		return err
	}
	if isVersion {
		fmt.Println(printVersion())
		return nil
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	baseLogger := log.New(os.Stdout, "", log.Ldate|log.Ltime)
	logger := customlogger.NewWithLevel(baseLogger, config.LogLevel, "")
	logger.Printf("Starting %s (mode: %s)", printVersion(), config.Mode)

	processorAdapter := processor.NewLocalProcessor(logger, filepath.Dir(config.ConfigFile))

	var watcherAdapter port.FileWatcher
	var pipelineService *pipeline.Service
	var proxyServer *proxy.Server

	parsedImmichURL, urlErr := url.Parse(config.ImmichURL)
	if urlErr != nil {
		return fmt.Errorf("invalid immich_url: %w", urlErr)
	}

	runWatcher, runProxy, _ := parseModes(config.Mode)

	if runWatcher {
		fsAdapter := filesystem.NewLocalFileSystem(config.WatchDir, config.UndoneDir)
		immichAdapter := immich.NewClient(config.ImmichURL, config.ImmichAPIKey, config.HTTPTimeoutSeconds, logger)

		var err error
		watcherAdapter, err = watcher.NewInotifyWatcher(config.WatchDir, logger, config.InotifyBufferSize)
		if err != nil {
			return fmt.Errorf("error creating file watcher: %w", err)
		}

		pipelineService = pipeline.NewService(
			watcherAdapter,
			processorAdapter,
			immichAdapter,
			fsAdapter,
			logger,
			config.Tasks.Tasks,
			config.MaxConcurrentRequests,
		)

		if err := watcherAdapter.Start(); err != nil {
			return fmt.Errorf("error starting watcher: %w", err)
		}
		pipelineService.Start()
	}

	if runProxy {
		proxyServer = proxy.NewServer(
			parsedImmichURL,
			config.BindAddr,
			config.FilterPath,
			config.FilterFormKey,
			processorAdapter,
			config.Tasks.Tasks,
			logger,
			config.MaxConcurrentRequests,
			config.HTTPTimeoutSeconds,
		)
		if err := proxyServer.Start(); err != nil {
			return fmt.Errorf("error starting proxy server: %w", err)
		}
	}

	// Wait for interrupt
	<-sigChan

	logger.Printf("Shutting down gracefully...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		if watcherAdapter != nil {
			watcherAdapter.Stop()
		}
		if pipelineService != nil {
			pipelineService.Stop()
		}
		if proxyServer != nil {
			_ = proxyServer.Stop(shutdownCtx)
		}
		close(done)
	}()

	select {
	case <-done:
		logger.Printf("Shutdown completed successfully")
	case <-shutdownCtx.Done():
		logger.Printf("Shutdown timeout exceeded, forcing exit")
	}

	return nil
}

func printVersion() string {
	return fmt.Sprintf("immich-optimizer %s, commit %s, built at %s", version, commit, date)
}
