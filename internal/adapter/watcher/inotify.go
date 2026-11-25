//go:build linux

package watcher

import (
	"errors"
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
	// linked is true when the file looks published with a hard link: it
	// had a second name when it was created, or a temporary file in the same
	// folder had just been closed and was gone already. Only such a file is
	// taken without a close event of its own; any other one may still be open
	// for writing (a writer stalled on a network share) and waits for it.
	linked bool
}

// closedInode is what was seen of a temporary file when it was closed. A file
// created later with the same inode number is the same file only if it still
// has this size and modification time: the kernel reuses inode numbers.
type closedInode struct {
	ts    time.Time
	size  int64
	mtime time.Time
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
	closedInodes   sync.Map // inode -> closedInode
	pendingCreates sync.Map // inode -> pendingCreate
	// goneTempCloses notes, per folder, when a temporary file was closed and
	// already gone when the event was handled (folder -> time.Time).
	goneTempCloses sync.Map
	stopChan       chan struct{}
	// rescanning is true while a rescan after a queue overflow runs;
	// rescanWanted asks for one more, for an overflow that came meanwhile.
	rescanning   atomic.Bool
	rescanWanted atomic.Bool
	// settle is how long a file must have been left alone before a scan
	// picks it up.
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

	go fw.scan(fw.watchDir)
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
	settled := time.NewTicker(fw.settle)
	defer settled.Stop()

	for {
		select {
		case <-fw.stopChan:
			return
		case <-settled.C:
			fw.takeSettledCreates()
		case <-ticker.C:
			now := time.Now()

			fw.closedInodes.Range(func(key, value any) bool {
				if ci, ok := value.(closedInode); ok && now.Sub(ci.ts) > time.Minute {
					fw.closedInodes.Delete(key)
				}
				return true
			})
			fw.goneTempCloses.Range(func(key, value any) bool {
				if ts, ok := value.(time.Time); ok && now.Sub(ts) > time.Minute {
					fw.goneTempCloses.Delete(key)
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

// takeSettledCreates takes the files published with a hard link whose
// temporary name was removed before its close event was handled (the inode
// could not be learnt then), once they have not been written to for the
// settle time. Other created files are left to their own close event: one
// that is still open may simply have a writer that stalled.
func (fw *inotifyWatcher) takeSettledCreates() {
	fw.pendingCreates.Range(func(key, value any) bool {
		pc, ok := value.(pendingCreate)
		if !ok || !pc.linked || time.Since(pc.ts) < fw.settle {
			return true
		}
		info, err := os.Stat(pc.path)
		switch {
		case err != nil || !info.Mode().IsRegular():
			fw.pendingCreates.Delete(key)
		case time.Since(info.ModTime()) >= fw.settle:
			fw.pendingCreates.Delete(key)
			fw.emitEvent(pc.path)
		}
		return true
	})
}

// addWatchRecursive watches dir and every folder below it. Only a failure on
// dir itself is returned: a folder below it that cannot be read or watched is
// logged and skipped, so that it does not leave the rest unwatched.
func (fw *inotifyWatcher) addWatchRecursive(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			fw.logger.Warnf("Not watching %s: %v", path, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if err := fw.addDirectoryWatch(path); err != nil {
			if path == dir {
				return err
			}
			if errors.Is(err, unix.ENOSPC) {
				fw.logger.Errorf("Not watching %s: the limit of inotify watches is reached (raise the sysctl fs.inotify.max_user_watches): %v", path, err)
			} else {
				fw.logger.Errorf("Not watching %s: %v", path, err)
			}
			return filepath.SkipDir
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

// scan emits the files under dir that no event will announce: those already
// there when the watch was added. A file written to a moment ago may still be
// open, so it is left alone and looked at again until it has settled; if it is
// still being written, its own close event announces it as well, and the
// pipeline ignores the second announcement.
func (fw *inotifyWatcher) scan(dir string) {
	emitted := make(map[string]struct{})
	for {
		unsettled := false
		// The callback never returns an error, so neither does the walk.
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				fw.logger.Printf("Error walking directory %s: %v", path, err)
				return nil
			}
			// A file under a temporary name is left to its rename or close
			// event.
			if d.IsDir() || utils.IsTempFile(path) {
				return nil
			}
			if _, done := emitted[path]; done {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil //nolint:nilerr // gone since it was listed: the walk goes on
			}
			// A date in the future says nothing about a write in progress.
			if age := time.Since(info.ModTime()); age >= 0 && age < fw.settle {
				unsettled = true
				return nil
			}
			emitted[path] = struct{}{}
			fw.emitEvent(path)
			return nil
		})
		if !unsettled {
			return
		}
		select {
		case <-fw.stopChan:
			return
		case <-time.After(fw.settle):
		}
	}
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
// events (queue overflow): it re-adds the watches and scans for files. An
// overflow that arrives while a rescan runs starts one more when it ends,
// because its dropped events may be for files the running one already passed.
func (fw *inotifyWatcher) rescan() {
	fw.rescanWanted.Store(true)
	if !fw.rescanning.CompareAndSwap(false, true) {
		return
	}
	go func() {
		for {
			for fw.rescanWanted.Swap(false) {
				select {
				case <-fw.stopChan:
					fw.rescanning.Store(false)
					return
				default:
				}
				_ = fw.addWatchRecursive(fw.watchDir)
				fw.scan(fw.watchDir)
			}
			fw.rescanning.Store(false)
			// An overflow between the last check and the line above found a
			// rescan running and left it to this one.
			if !fw.rescanWanted.Load() || !fw.rescanning.CompareAndSwap(false, true) {
				return
			}
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

	// An event for a watch that is not known (any more) has no folder to
	// resolve its name against.
	if name == "" || watchedDir == "" {
		return
	}

	filePath := filepath.Join(watchedDir, name)
	fw.logger.Debugf("Inotify raw event 0x%x for %s", event.Mask, filePath)

	if info, err := os.Stat(filePath); err == nil && info.IsDir() {
		if event.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 {
			fw.logger.Debugf("New directory detected (%s), adding recursive watches", filePath)
			if err := fw.addWatchRecursive(filePath); err != nil {
				fw.logger.Errorf("Not watching %s: %v", filePath, err)
			}
			go fw.scan(filePath)
		}
		return
	}

	if event.Mask&unix.IN_CREATE != 0 {
		if info, err := os.Stat(filePath); err == nil && !info.IsDir() && !utils.IsTempFile(filePath) {
			if statT, ok := info.Sys().(*syscall.Stat_t); ok {
				v, noted := fw.closedInodes.LoadAndDelete(statT.Ino)
				ci, _ := v.(closedInode)
				sameSize := noted && ci.size == info.Size()
				if sameSize && ci.mtime.Equal(info.ModTime()) {
					fw.emitEvent(filePath)
				} else {
					linked := statT.Nlink > 1
					// Same inode and size as a temporary file just closed,
					// but another mtime: a client that sets the capture
					// time back before linking (SFTP with preserved times).
					// The note may also be stale for a reused inode, so
					// the file is only taken once it has settled.
					if sameSize {
						linked = true
					}
					if v, ok := fw.goneTempCloses.Load(watchedDir); ok && time.Since(v.(time.Time)) < fw.settle {
						linked = true
					}
					fw.pendingCreates.Store(statT.Ino, pendingCreate{
						path:   filePath,
						ts:     time.Now(),
						linked: linked,
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
						fw.closedInodes.Store(statT.Ino, closedInode{ts: time.Now(), size: info.Size(), mtime: info.ModTime()})
					}
				}
			} else {
				fw.goneTempCloses.Store(watchedDir, time.Now())
			}
			// A temporary file is never emitted under its own name, also
			// when it is already gone (renamed into place) by now.
			return
		}
		// The file is announced under its own name: forget what was noted
		// about its inode, or a later file that gets the same inode number
		// would be mistaken for this one.
		if info, err := os.Stat(filePath); err == nil {
			if statT, ok := info.Sys().(*syscall.Stat_t); ok {
				fw.closedInodes.Delete(statT.Ino)
				fw.pendingCreates.Delete(statT.Ino)
			}
		}
		fw.emitEvent(filePath)
	}
}

func (fw *inotifyWatcher) emitEvent(path string) {
	fw.logger.Debugf("Emitting file event for processing: %s", path)
	select {
	case fw.events <- entity.FileEvent{Path: path, Timestamp: time.Now()}:
	case <-fw.stopChan:
	}
}
