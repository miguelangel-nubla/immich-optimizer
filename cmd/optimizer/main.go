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
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/watcher"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
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
	viper.BindEnv("immich_url")
	viper.BindEnv("immich_api_key")
	viper.BindEnv("watch_dir")
	viper.BindEnv("undone_dir")
	viper.BindEnv("tasks_file")

	viper.SetDefault("immich_url", "")
	viper.SetDefault("immich_api_key", "")
	viper.SetDefault("watch_dir", "/watch")
	viper.SetDefault("undone_dir", "/undone")
	viper.SetDefault("tasks_file", "tasks.yaml")

	fs := flag.NewFlagSet("immich-optimizer", flag.ContinueOnError)
	fs.BoolVar(&ac.ShowVersion, "version", false, "Show the current version")
	fs.StringVar(&ac.ImmichURL, "immich_url", viper.GetString("immich_url"), "Immich server URL")
	fs.StringVar(&ac.ImmichAPIKey, "immich_api_key", viper.GetString("immich_api_key"), "Immich API key")
	fs.StringVar(&ac.WatchDir, "watch_dir", viper.GetString("watch_dir"), "Directory to watch for new files")
	fs.StringVar(&ac.UndoneDir, "undone_dir", viper.GetString("undone_dir"), "Directory to copy files that failed")
	fs.StringVar(&ac.ConfigFile, "tasks_file", viper.GetString("tasks_file"), "Path to the configuration file")

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

func validateAppConfig(ac *entity.AppConfig) error {
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

	if ac.ImmichAPIKey == "" {
		return fmt.Errorf("the -immich_api_key flag is required")
	}

	if len(strings.TrimSpace(ac.ImmichAPIKey)) < 10 {
		return fmt.Errorf("immich_api_key appears to be too short")
	}

	if ac.ConfigFile == "" {
		return fmt.Errorf("the -tasks_file flag is required")
	}

	if mkdirErr := os.MkdirAll(ac.WatchDir, 0750); mkdirErr != nil {
		return fmt.Errorf("error creating watch directory: %v", mkdirErr)
	}

	if mkdirErr := os.MkdirAll(ac.UndoneDir, 0750); mkdirErr != nil {
		return fmt.Errorf("error creating undone directory: %v", mkdirErr)
	}

	var err error
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
	logger := customlogger.New(baseLogger, "")
	logger.Printf("Starting %s", printVersion())

	// Initialize Adapters
	fsAdapter := filesystem.NewLocalFileSystem(config.WatchDir, config.UndoneDir)
	immichAdapter := immich.NewClient(config.ImmichURL, config.ImmichAPIKey, config.HTTPTimeoutSeconds, logger)
	processorAdapter := processor.NewLocalProcessor(logger, filepath.Dir(config.ConfigFile))

	watcherAdapter, err := watcher.NewInotifyWatcher(config.WatchDir, logger, config.InotifyBufferSize)
	if err != nil {
		return fmt.Errorf("error creating file watcher: %w", err)
	}

	// Initialize Pipeline Service (Usecase)
	pipelineService := pipeline.NewService(
		watcherAdapter,
		processorAdapter,
		immichAdapter,
		fsAdapter,
		logger,
		config.Tasks.Tasks,
		config.MaxConcurrentRequests,
	)

	// Start everything
	if err := watcherAdapter.Start(); err != nil {
		return fmt.Errorf("error starting watcher: %w", err)
	}
	pipelineService.Start()

	// Wait for interrupt
	<-sigChan

	logger.Printf("Shutting down gracefully...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	done := make(chan struct{})
	go func() {
		watcherAdapter.Stop()
		pipelineService.Stop()
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
