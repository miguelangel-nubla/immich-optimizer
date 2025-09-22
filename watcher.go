package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

type FileWatcher struct {
	fd           int
	watchDir     string
	immichClient *ImmichClient
	config       *Config
	logger       *log.Logger
	watchMap     map[string]int
	bufferSize   int
	appConfig    *AppConfig
}

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

func (fw *FileWatcher) Stop() {
	for _, wd := range fw.watchMap {
		unix.InotifyRmWatch(fw.fd, uint32(wd))
	}
	unix.Close(fw.fd)
}

func (fw *FileWatcher) addWatchRecursive(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			wd, err := unix.InotifyAddWatch(fw.fd, path, unix.IN_CLOSE_WRITE|unix.IN_MOVED_TO|unix.IN_CREATE)
			if err != nil {
				return fmt.Errorf("failed to add watch for %s: %w", path, err)
			}
			fw.watchMap[path] = wd
			fw.logger.Printf("Added watch for directory: %s", path)
		}

		return nil
	})
}

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

func (fw *FileWatcher) watchLoop() {
	buf := make([]byte, fw.bufferSize)

	for {
		n, err := unix.Read(fw.fd, buf)
		if err != nil {
			fw.logger.Printf("Error reading inotify events: %v", err)
			return
		}

		offset := 0
		for offset < n {
			event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			nameBytes := buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+int(event.Len)]
			name := strings.TrimRight(string(nameBytes), "\x00")

			// Find the directory path for this watch descriptor
			var watchedDir string
			for dir, wd := range fw.watchMap {
				if wd == int(event.Wd) {
					watchedDir = dir
					break
				}
			}

			if event.Mask&unix.IN_CREATE != 0 && name != "" {
				// New directory created, add watch for it
				newPath := filepath.Join(watchedDir, name)
				if info, err := os.Stat(newPath); err == nil && info.IsDir() {
					fw.addWatchRecursive(newPath)
				}
			}

			if event.Mask&unix.IN_CLOSE_WRITE != 0 || event.Mask&unix.IN_MOVED_TO != 0 {
				if name != "" && watchedDir != "" {
					filePath := filepath.Join(watchedDir, name)
					fw.processFile(filePath)
				}
			}

			offset += unix.SizeofInotifyEvent + int(event.Len)
		}
	}
}

func (fw *FileWatcher) processFile(filePath string) {
	info, err := os.Stat(filePath)
	if err != nil {
		fw.logger.Printf("Error getting file info for %s: %v", filePath, err)
		return
	}

	if info.IsDir() {
		return
	}

	fw.logger.Printf("Processing file: %s", filePath)

	extension := filepath.Ext(filePath)
	if !shouldProcessExtension(extension, fw.config.Tasks) {
		fw.logger.Printf("Skipping file %s (extension %s not configured for processing)", filePath, extension)
		fw.uploadToImmich(filePath)
		return
	}

	// Process the file using existing TaskProcessor
	tp, err := NewTaskProcessor(filePath)
	if err != nil {
		fw.logger.Printf("Error creating task processor for %s: %v", filePath, err)
		return
	}
	defer tp.Close()

	jobLogger := newCustomLogger(fw.logger, fmt.Sprintf("file %s: ", filepath.Base(filePath)))
	tp.SetLogger(jobLogger)
	if fw.appConfig != nil {
		tp.SetSemaphore(fw.appConfig.Semaphore)
		tp.SetConfigDir(filepath.Dir(fw.appConfig.ConfigFile))
	}

	err = tp.Process(fw.config.Tasks)
	if err != nil {
		fw.logger.Printf("Error processing file %s: %v", filePath, err)
		// Upload original file if processing fails
		fw.uploadToImmich(filePath)
		return
	}

	// Determine which file to upload based on processing results
	if tp.ProcessedFile != nil && tp.ProcessedSize > 0 && tp.OriginalSize > tp.ProcessedSize {
		// Upload processed file if it's smaller and not empty
		processedPath, err := tp.GetProcessedFilePath()
		if err != nil {
			fw.logger.Printf("Error getting processed file path: %v", err)
			fw.uploadToImmich(filePath)
			return
		}
		fw.logger.Printf("Optimized file uploaded: %s -> %s",
			humanReadableSize(tp.OriginalSize),
			humanReadableSize(tp.ProcessedSize))
		fw.uploadToImmich(processedPath)
	} else {
		// Upload original file if no optimization or optimization didn't reduce size
		fw.logger.Printf("Original file uploaded (no optimization achieved)")
		fw.uploadToImmich(filePath)
	}
}

func (fw *FileWatcher) uploadToImmich(filePath string) {
	err := fw.immichClient.UploadAsset(filePath)
	if err != nil {
		fw.logger.Printf("Error uploading file %s to Immich: %v", filePath, err)
		return
	}

	// Remove the file after successful upload
	if err := os.Remove(filePath); err != nil {
		fw.logger.Printf("Error removing file %s after upload: %v", filePath, err)
	}
}
