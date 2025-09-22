package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/viper"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type AppConfig struct {
	ShowVersion           bool
	ImmichURL             string
	ImmichAPIKey          string
	WatchDir              string
	ConfigFile            string
	MaxConcurrentRequests int
	HTTPTimeoutSeconds    int
	InotifyBufferSize     int
	Semaphore             chan struct{}
	Tasks                 *Config
}

func NewAppConfig() *AppConfig {
	maxConcurrent := 10
	return &AppConfig{
		MaxConcurrentRequests: maxConcurrent,
		HTTPTimeoutSeconds:    120,
		InotifyBufferSize:     8192, // 8KB buffer for better performance
		Semaphore:             make(chan struct{}, maxConcurrent),
	}
}

var appConfig *AppConfig

func init() {
	appConfig = NewAppConfig()

	viper.SetEnvPrefix("iuo")
	viper.AutomaticEnv()
	viper.BindEnv("immich_url")
	viper.BindEnv("immich_api_key")
	viper.BindEnv("watch_dir")
	viper.BindEnv("tasks_file")

	viper.SetDefault("immich_url", "")
	viper.SetDefault("immich_api_key", "")
	viper.SetDefault("watch_dir", "/watch")
	viper.SetDefault("tasks_file", "tasks.yaml")

	flag.BoolVar(&appConfig.ShowVersion, "version", false, "Show the current version")
	flag.StringVar(&appConfig.ImmichURL, "immich_url", viper.GetString("immich_url"), "Immich server URL. Example: http://immich-server:2283")
	flag.StringVar(&appConfig.ImmichAPIKey, "immich_api_key", viper.GetString("immich_api_key"), "Immich API key")
	flag.StringVar(&appConfig.WatchDir, "watch_dir", viper.GetString("watch_dir"), "Directory to watch for new files")
	flag.StringVar(&appConfig.ConfigFile, "tasks_file", viper.GetString("tasks_file"), "Path to the configuration file")
	flag.Parse()

	if appConfig.ShowVersion {
		fmt.Println(printVersion())
		os.Exit(0)
	}

	if err := appConfig.validate(); err != nil {
		log.Fatal(err)
	}
}

func (ac *AppConfig) validate() error {
	if ac.ImmichURL == "" {
		return fmt.Errorf("the -immich_url flag is required")
	}

	// Validate URL format
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

	// Basic API key validation (should be a non-empty string with reasonable length)
	if len(strings.TrimSpace(ac.ImmichAPIKey)) < 10 {
		return fmt.Errorf("immich_api_key appears to be too short (minimum 10 characters)")
	}

	if ac.ConfigFile == "" {
		return fmt.Errorf("the -tasks_file flag is required")
	}

	// Create watch directory if it doesn't exist
	if mkdirErr := os.MkdirAll(ac.WatchDir, 0750); mkdirErr != nil {
		return fmt.Errorf("error creating watch directory: %v", mkdirErr)
	}

	var err error
	ac.Tasks, err = NewConfig(&ac.ConfigFile)
	if err != nil {
		return fmt.Errorf("error loading config file: %v", err)
	}

	return nil
}

func main() {
	config := appConfig

	baseLogger := log.New(os.Stdout, "", log.Ldate|log.Ltime)
	customLogger := newCustomLogger(baseLogger, "")
	customLogger.Printf("Starting %s", printVersion())

	// Create Immich client
	immichClient := NewImmichClient(config.ImmichURL, config.ImmichAPIKey, config.HTTPTimeoutSeconds, customLogger)

	// Create file watcher
	watcher, err := NewFileWatcher(config.WatchDir, immichClient, config.Tasks, baseLogger, config.InotifyBufferSize)
	if err != nil {
		customLogger.Printf("Error creating file watcher: %v", err)
		os.Exit(1)
	}
	defer watcher.Stop()

	// Start watching
	err = watcher.Start(config)
	if err != nil {
		customLogger.Printf("Error starting file watcher: %v", err)
		os.Exit(1)
	}

	// Setup graceful shutdown

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	customLogger.Printf("Service started. Press Ctrl+C to stop.")

	// Block until we receive our signal
	<-sigChan

	customLogger.Printf("Shutting down gracefully...")

	// Create a deadline to wait for
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	// Stop the watcher gracefully
	done := make(chan struct{})
	go func() {
		watcher.Stop()
		close(done)
	}()

	select {
	case <-done:
		customLogger.Printf("Shutdown completed successfully")
	case <-shutdownCtx.Done():
		customLogger.Printf("Shutdown timeout exceeded, forcing exit")
	}
}

func printVersion() string {
	return fmt.Sprintf("immich-optimizer %s, commit %s, built at %s", version, commit, date)
}
