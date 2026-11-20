//go:build linux

package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
	"golang.org/x/sys/unix"
)

const (
	inotifyWatchMask = unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_CREATE
)

type pendingCreate struct {
	path string
	ts   time.Time
}

type inotifyWatcher struct {
	fd             int
	watchDir       string
	logger         *logger.Logger
	bufferSize     int
	watchMapMu     sync.RWMutex
	watchMap       map[string]int
	watchDirMap    map[int]string
	events         chan entity.FileEvent
	closedInodes   sync.Map
	pendingCreates sync.Map
	stopChan       chan struct{}
	// rescanning is 1 while a rescan after a queue overflow is scheduled.
	rescanning atomic.Bool
	// settle is how recently a file may have been written and still be
	// picked up by a rescan.
	settle time.Duration
}

func NewInotifyWatcher(watchDir string, log *logger.Logger, bufferSize int) (port.FileWatcher, error) {
	fd, err := unix.InotifyInit()
	if err != nil {
		return nil, fmt.Errorf("failed to create inotify instance: %w", err)
	}

	return &inotifyWatcher{
		fd:          fd,
		watchDir:    watchDir,
		logger:      log,
		bufferSize:  bufferSize,
		watchMap:    make(map[string]int),
		watchDirMap: make(map[int]string),
		events:      make(chan entity.FileEvent, 100),
		stopChan:    make(chan struct{}),
		settle:      10 * time.Second,
	}, nil
}

func (fw *inotifyWatcher) Events() <-chan entity.FileEvent {
	return fw.events
}

func (fw *inotifyWatcher) Start() error {
	fw.logger.Printf("Starting recursive file watcher on directory: %s", fw.watchDir)

	if err := fw.addWatchRecursive(fw.watchDir); err != nil {
		return fmt.Errorf("failed to add recursive watches: %w", err)
	}

	go fw.processExistingFilesRecursive(fw.watchDir)
	go fw.cleanupLoop()
	go fw.watchLoop()

	return nil
}

func (fw *inotifyWatcher) Stop() error {
	close(fw.stopChan)
	fw.watchMapMu.Lock()
	for dir, wd := range fw.watchMap {
		unix.InotifyRmWatch(fw.fd, uint32(wd))
		delete(fw.watchMap, dir)
		delete(fw.watchDirMap, wd)
	}
	fw.watchMapMu.Unlock()
	unix.Close(fw.fd)
	return nil
}

func (fw *inotifyWatcher) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-fw.stopChan:
			return
		case <-ticker.C:
			now := time.Now()

			fw.closedInodes.Range(func(key, value any) bool {
				if ts, ok := value.(time.Time); ok && now.Sub(ts) > time.Minute {
					fw.closedInodes.Delete(key)
				}
				return true
			})

			fw.pendingCreates.Range(func(key, value any) bool {
				if pc, ok := value.(pendingCreate); ok && now.Sub(pc.ts) > time.Minute {
					fw.pendingCreates.Delete(key)
				}
				return true
			})
		}
	}
}

func (fw *inotifyWatcher) addWatchRecursive(dir string) error {
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

func (fw *inotifyWatcher) addDirectoryWatch(path string) error {
	wd, err := unix.InotifyAddWatch(fw.fd, path, inotifyWatchMask)
	if err != nil {
		return fmt.Errorf("failed to add watch for %s: %w", path, err)
	}

	fw.watchMapMu.Lock()
	if oldPath, exists := fw.watchDirMap[wd]; exists && oldPath != path {
		delete(fw.watchMap, oldPath)
	}
	fw.watchMap[path] = wd
	fw.watchDirMap[wd] = path
	fw.watchMapMu.Unlock()

	fw.logger.Printf("Watching new directory: %s", path)
	return nil
}

func (fw *inotifyWatcher) processExistingFilesRecursive(dir string) {
	// The callback never returns an error, so neither does the walk.
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			fw.logger.Printf("Error walking directory %s: %v", path, err)
			return nil
		}
		// A scan can catch a file that is still being written under a
		// temporary name: leave it to its own rename or close event.
		if !d.IsDir() && !utils.IsTempFile(path) {
			fw.emitEvent(path)
		}
		return nil
	})
}

func (fw *inotifyWatcher) watchLoop() {
	buf := make([]byte, fw.bufferSize)
	backoff := 100 * time.Millisecond
	for {
		select {
		case <-fw.stopChan:
			return
		default:
			n, err := unix.Read(fw.fd, buf)
			if err != nil {
				select {
				case <-fw.stopChan:
					return
				default:
				}
				if err == unix.EINTR {
					continue
				}
				// Back off instead of logging the same error ten times a
				// second for as long as it lasts.
				fw.logger.Errorf("Error reading inotify events (retrying in %s): %v", backoff, err)
				select {
				case <-fw.stopChan:
					return
				case <-time.After(backoff):
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
				continue
			}
			backoff = 100 * time.Millisecond
			fw.processInotifyEvents(buf, n)
		}
	}
}

func (fw *inotifyWatcher) processInotifyEvents(buf []byte, n int) {
	offset := 0
	for offset < n {
		if offset+unix.SizeofInotifyEvent > n {
			break
		}
		event := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		if offset+unix.SizeofInotifyEvent+int(event.Len) > n {
			break
		}
		nameBytes := buf[offset+unix.SizeofInotifyEvent : offset+unix.SizeofInotifyEvent+int(event.Len)]
		name := strings.TrimRight(string(nameBytes), "\x00")

		watchedDir := fw.findWatchedDirectory(int(event.Wd))
		fw.handleInotifyEvent(event, name, watchedDir)

		offset += unix.SizeofInotifyEvent + int(event.Len)
	}
}

func (fw *inotifyWatcher) findWatchedDirectory(wd int) string {
	fw.watchMapMu.RLock()
	defer fw.watchMapMu.RUnlock()
	return fw.watchDirMap[wd]
}

// rescan walks the whole watch directory again after the kernel dropped
// events (queue overflow): it re-adds the watches and emits every file that
// has not been written to for a moment. Files still being written are left to
// their own close event, and a second pass catches any whose event was among
// the dropped ones.
func (fw *inotifyWatcher) rescan() {
	if !fw.rescanning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer fw.rescanning.Store(false)
		for pass := 0; pass < 2; pass++ {
			select {
			case <-fw.stopChan:
				return
			case <-time.After(time.Duration(pass) * 2 * fw.settle):
			}
			_ = fw.addWatchRecursive(fw.watchDir)
			_ = filepath.WalkDir(fw.watchDir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil
				}
				if utils.IsTempFile(path) {
					return nil
				}
				if info, err := d.Info(); err == nil && time.Since(info.ModTime()) >= fw.settle {
					fw.emitEvent(path)
				}
				return nil
			})
		}
	}()
}

func (fw *inotifyWatcher) handleInotifyEvent(event *unix.InotifyEvent, name, watchedDir string) {
	if event.Mask&unix.IN_Q_OVERFLOW != 0 {
		fw.logger.Warnf("The kernel dropped file events (inotify queue overflow): scanning %s again", fw.watchDir)
		fw.rescan()
		return
	}
	if event.Mask&unix.IN_IGNORED != 0 {
		fw.watchMapMu.Lock()
		if dir, exists := fw.watchDirMap[int(event.Wd)]; exists {
			delete(fw.watchMap, dir)
			delete(fw.watchDirMap, int(event.Wd))
		}
		fw.watchMapMu.Unlock()
		return
	}

	if name == "" {
		return
	}

	filePath := filepath.Join(watchedDir, name)
	fw.logger.Debugf("Inotify raw event 0x%x for %s", event.Mask, filePath)

	if info, err := os.Stat(filePath); err == nil && info.IsDir() {
		if event.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
			fw.logger.Debugf("New directory detected (%s), adding recursive watches", filePath)
			_ = fw.addWatchRecursive(filePath)
			go fw.processExistingFilesRecursive(filePath)
		}
		return
	}

	if event.Mask&unix.IN_CREATE != 0 {
		if info, err := os.Stat(filePath); err == nil && !info.IsDir() && !utils.IsTempFile(filePath) {
			if statT, ok := info.Sys().(*syscall.Stat_t); ok {
				if _, loaded := fw.closedInodes.LoadAndDelete(statT.Ino); loaded {
					if watchedDir != "" {
						fw.emitEvent(filePath)
					}
				} else if watchedDir != "" {
					fw.pendingCreates.Store(statT.Ino, pendingCreate{
						path: filePath,
						ts:   time.Now(),
					})
				}
			}
		}
	}

	if event.Mask&unix.IN_CLOSE_WRITE != 0 || event.Mask&unix.IN_MOVED_TO != 0 {
		if event.Mask&unix.IN_CLOSE_WRITE != 0 && utils.IsTempFile(filePath) {
			if info, err := os.Stat(filePath); err == nil {
				if statT, ok := info.Sys().(*syscall.Stat_t); ok {
					if pendingV, found := fw.pendingCreates.LoadAndDelete(statT.Ino); found {
						if pc, ok := pendingV.(pendingCreate); ok {
							fw.emitEvent(pc.path)
						}
					} else {
						fw.closedInodes.Store(statT.Ino, time.Now())
					}
				}
			}
			// A temporary file is never emitted under its own name, also
			// when it is already gone (renamed into place) by now.
			return
		}
		if watchedDir != "" {
			fw.emitEvent(filePath)
		}
	}
}

func (fw *inotifyWatcher) emitEvent(path string) {
	fw.logger.Debugf("Emitting file event for processing: %s", path)
	select {
	case fw.events <- entity.FileEvent{Path: path, Timestamp: time.Now()}:
	case <-fw.stopChan:
	}
}
