package main

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// watchLoop monitors for inotify events in a continuous loop
func (fw *FileWatcher) watchLoop() {
	buf := make([]byte, fw.bufferSize)

	for {
		n, err := unix.Read(fw.fd, buf)
		if err != nil {
			fw.logger.Printf("Error reading inotify events: %v", err)
			return
		}

		fw.processInotifyEvents(buf, n)
	}
}

// processInotifyEvents processes a buffer of inotify events
func (fw *FileWatcher) processInotifyEvents(buf []byte, n int) {
	offset := 0
	for offset < n {
		event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		nameBytes := buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+int(event.Len)]
		name := strings.TrimRight(string(nameBytes), "\x00")

		watchedDir := fw.findWatchedDirectory(int(event.Wd))
		fw.handleInotifyEvent(event, name, watchedDir)

		offset += unix.SizeofInotifyEvent + int(event.Len)
	}
}

// findWatchedDirectory finds the directory path for a given watch descriptor
func (fw *FileWatcher) findWatchedDirectory(wd int) string {
	for dir, watchDescriptor := range fw.watchMap {
		if watchDescriptor == wd {
			return dir
		}
	}
	return ""
}

// handleInotifyEvent processes a single inotify event
func (fw *FileWatcher) handleInotifyEvent(event *unix.InotifyEvent, name, watchedDir string) {
	if name == "" {
		return
	}

	filePath := filepath.Join(watchedDir, name)

	if event.Mask&unix.IN_CREATE != 0 {
		fw.handleDirectoryCreation(filePath)
	}

	if event.Mask&unix.IN_CLOSE_WRITE != 0 || event.Mask&unix.IN_MOVED_TO != 0 {
		if watchedDir != "" {
			fw.processFile(filePath)
		}
	}
}

// handleDirectoryCreation handles the creation of new directories
func (fw *FileWatcher) handleDirectoryCreation(path string) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		fw.addWatchRecursive(path)
	}
}