package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	defaultBufferSize = 4096
	inotifyWatchMask  = unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_CREATE
)

// FileWatcher monitors directory changes using inotify and processes files
type FileWatcher struct {
	fd           int            // inotify file descriptor
	watchDir     string         // root directory to watch
	immichClient *ImmichClient  // client for uploading to Immich
	config       *Config        // processing configuration
	logger       *log.Logger    // logger instance
	watchMap     map[string]int // maps directory paths to watch descriptors
	bufferSize   int            // buffer size for reading inotify events
	appConfig    *AppConfig     // application configuration
}

// NewFileWatcher creates a new file watcher instance
func NewFileWatcher(watchDir string, immichClient *ImmichClient, config *Config, logger *log.Logger, bufferSize int) (*FileWatcher, error) {
	fd, err := unix.InotifyInit()
	if err != nil {
		return nil, fmt.Errorf("failed to create inotify instance: %w", err)
	}

	fw := &FileWatcher{
		fd:           fd,
		watchDir:     watchDir,
		immichClient: immichClient,
		config:       config,
		logger:       logger,
		watchMap:     make(map[string]int),
		bufferSize:   bufferSize,
	}

	return fw, nil
}

// Start begins monitoring the directory for file changes
func (fw *FileWatcher) Start(config *AppConfig) error {
	fw.appConfig = config
	fw.logger.Printf("Starting recursive file watcher on directory: %s", fw.watchDir)

	// Add watches recursively
	err := fw.addWatchRecursive(fw.watchDir)
	if err != nil {
		return fmt.Errorf("failed to add recursive watches: %w", err)
	}

	// Process existing files in all directories
	fw.processExistingFilesRecursive(fw.watchDir)

	// Start watching for new files
	go fw.watchLoop()

	return nil
}

// Stop closes the file watcher and cleans up resources
func (fw *FileWatcher) Stop() {
	for _, wd := range fw.watchMap {
		unix.InotifyRmWatch(fw.fd, uint32(wd))
	}
	unix.Close(fw.fd)
}

// addWatchRecursive adds inotify watches to all directories recursively
func (fw *FileWatcher) addWatchRecursive(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return fw.addDirectoryWatch(path)
		}

		return nil
	})
}

// addDirectoryWatch adds an inotify watch to a specific directory
func (fw *FileWatcher) addDirectoryWatch(path string) error {
	wd, err := unix.InotifyAddWatch(fw.fd, path, inotifyWatchMask)
	if err != nil {
		return fmt.Errorf("failed to add watch for %s: %w", path, err)
	}
	fw.watchMap[path] = wd
	fw.logger.Printf("Added watch for directory: %s", path)
	return nil
}

// processExistingFilesRecursive processes all existing files in the directory
func (fw *FileWatcher) processExistingFilesRecursive(dir string) {
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			fw.logger.Printf("Error walking directory %s: %v", path, err)
			return nil
		}

		if !d.IsDir() {
			fw.processFile(path)
		}

		return nil
	})
}
