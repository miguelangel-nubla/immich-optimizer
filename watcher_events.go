package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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

	// Debug log to confirm what raw events are produced (especially for rename operations)
	fw.logger.Printf("[DEBUG] raw inotify event: mask=0x%x, name=%s, watchedDir=%s", event.Mask, name, watchedDir)

	if event.Mask&unix.IN_CREATE != 0 {
		fw.handleDirectoryCreation(filePath)

		// Rescue hardlinks / atomic creations sent by sync clients (like FolderSync)
		// Because they only emit IN_CREATE without IN_MOVED_TO or IN_CLOSE_WRITE, we identify 
		// them by matching their inode to a temporary file that just finished writing.
		if info, err := os.Stat(filePath); err == nil && !info.IsDir() && !fw.isTempFile(filePath) {
			if statT, ok := info.Sys().(*syscall.Stat_t); ok {
				if _, loaded := fw.closedInodes.LoadAndDelete(statT.Ino); loaded {
					// Matched a previous IN_CLOSE_WRITE
					if watchedDir != "" {
						go fw.processFile(filePath)
					}
				} else if watchedDir != "" {
					// Arrived before IN_CLOSE_WRITE, cache it for the central sweeper
					fw.pendingCreates.Store(statT.Ino, pendingCreate{
						path: filePath,
						ts:   time.Now(),
					})
				}
			}
		}
	}

	if event.Mask&unix.IN_CLOSE_WRITE != 0 || event.Mask&unix.IN_MOVED_TO != 0 {
		// If a temporary file finishes writing, store its inode to check against future IN_CREATE linkings
		if event.Mask&unix.IN_CLOSE_WRITE != 0 && fw.isTempFile(filePath) {
			if info, err := os.Stat(filePath); err == nil {
				if statT, ok := info.Sys().(*syscall.Stat_t); ok {
					if pendingV, found := fw.pendingCreates.LoadAndDelete(statT.Ino); found {
						// Arrived after IN_CREATE
						if pc, ok := pendingV.(pendingCreate); ok {
							go fw.processFile(pc.path)
						}
					} else {
						// Store for the sweeper loop in case IN_CREATE comes later
						fw.closedInodes.Store(statT.Ino, time.Now())
					}
					// Return explicitly to prevent a double processing attempt here below
					return
				}
			}
		}

		if watchedDir != "" {
			go fw.processFile(filePath)
		}
	}
}

// handleDirectoryCreation handles the creation of new directories
func (fw *FileWatcher) handleDirectoryCreation(path string) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		fw.addWatchRecursive(path)
		go fw.processExistingFilesRecursive(path) // catch any files missed during inotify race conditions
	}
}
