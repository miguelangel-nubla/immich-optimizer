//go:build linux

package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"golang.org/x/sys/unix"
)

func startWatcher(t *testing.T) (*inotifyWatcher, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewInotifyWatcher(dir, customlogger.NewWithLevel(nil, "error", ""), 8192)
	if err != nil {
		t.Fatal(err)
	}
	fw := w.(*inotifyWatcher)
	fw.settle = 200 * time.Millisecond
	if err := fw.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fw.Stop() })
	return fw, dir
}

// collect returns the distinct paths emitted within d.
func collect(fw *inotifyWatcher, d time.Duration) map[string]bool {
	got := map[string]bool{}
	deadline := time.After(d)
	for {
		select {
		case e := <-fw.Events():
			got[e.Path] = true
		case <-deadline:
			return got
		}
	}
}

func TestInotifyEmitsWrittenRenamedAndNestedFiles(t *testing.T) {
	fw, dir := startWatcher(t)

	plain := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Written under a temporary name, then renamed into place.
	tmp := filepath.Join(dir, "b.jpg.part")
	renamed := filepath.Join(dir, "b.jpg")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, renamed); err != nil {
		t.Fatal(err)
	}
	// A folder created later is watched too.
	sub := filepath.Join(dir, "trip")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	nested := filepath.Join(sub, "c.jpg")
	if err := os.WriteFile(nested, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := collect(fw, 2*time.Second)
	for _, want := range []string{plain, renamed, nested} {
		if !got[want] {
			t.Errorf("no event for %s (got %v)", want, got)
		}
	}
	if got[tmp] {
		t.Errorf("the temporary name %s was emitted", tmp)
	}
}

func TestInotifyEmitsFilesPresentAtStart(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "old.jpg")
	if err := os.WriteFile(existing, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewInotifyWatcher(dir, customlogger.NewWithLevel(nil, "error", ""), 8192)
	if err != nil {
		t.Fatal(err)
	}
	fw := w.(*inotifyWatcher)
	fw.settle = 300 * time.Millisecond
	if err := fw.Start(); err != nil {
		t.Fatal(err)
	}
	defer fw.Stop()
	// Written a moment ago: it may still be open, so not yet.
	if got := collect(fw, 100*time.Millisecond); got[existing] {
		t.Errorf("a file written just now was emitted without waiting: %v", got)
	}
	if got := collect(fw, 2*time.Second); !got[existing] {
		t.Errorf("file present at start not emitted: %v", got)
	}
}

// A folder that cannot be read must not leave the folders after it unwatched.
func TestInotifyUnreadableFolderDoesNotStopTheRest(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every folder")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "a-locked")
	later := filepath.Join(dir, "b-later")
	for _, d := range []string{filepath.Join(locked, "inner"), later} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o755)
	w, err := NewInotifyWatcher(dir, customlogger.NewWithLevel(nil, "error", ""), 8192)
	if err != nil {
		t.Fatal(err)
	}
	fw := w.(*inotifyWatcher)
	fw.settle = 200 * time.Millisecond
	if err := fw.Start(); err != nil {
		t.Fatalf("an unreadable folder stopped the start: %v", err)
	}
	defer fw.Stop()
	f := filepath.Join(later, "c.jpg")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := collect(fw, time.Second); !got[f] {
		t.Errorf("no event from the folder after the unreadable one: %v", got)
	}
}

// When the kernel drops events, every settled file is found again by a
// rescan, including one in a folder whose creation was among the lost events.
func TestInotifyQueueOverflowTriggersRescan(t *testing.T) {
	fw, dir := startWatcher(t)
	collect(fw, 300*time.Millisecond)

	// Simulate lost events: stop watching, create things, then report the
	// overflow the way the kernel does.
	fw.watchMapMu.Lock()
	for wd := range fw.watchDirMap {
		_, _ = unix.InotifyRmWatch(fw.fd, uint32(wd))
	}
	fw.watchMapMu.Unlock()
	lost := filepath.Join(dir, "lost.jpg")
	lostNested := filepath.Join(dir, "new-folder", "lost2.jpg")
	if err := os.WriteFile(lost, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(lostNested), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lostNested, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	collect(fw, 300*time.Millisecond) // nothing may have noticed them
	time.Sleep(fw.settle)

	fw.handleInotifyEvent(&unix.InotifyEvent{Wd: -1, Mask: unix.IN_Q_OVERFLOW}, "", "")
	got := collect(fw, 2*time.Second)
	if !got[lost] || !got[lostNested] {
		t.Errorf("rescan missed files: %v", got)
	}

	// And watching works again afterwards.
	after := filepath.Join(dir, "new-folder", "after.jpg")
	if err := os.WriteFile(after, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := collect(fw, 2*time.Second); !got[after] {
		t.Errorf("no event after the rescan: %v", got)
	}
}

// collectTimed returns when each path was first emitted, within d.
func collectTimed(fw *inotifyWatcher, d time.Duration) map[string]time.Time {
	got := map[string]time.Time{}
	deadline := time.After(d)
	for {
		select {
		case e := <-fw.Events():
			if _, seen := got[e.Path]; !seen {
				got[e.Path] = time.Now()
			}
		case <-deadline:
			return got
		}
	}
}

// writeSlowly writes a file in pieces over a while, the way a copy of a large
// video arrives, and returns when it closed it.
func writeSlowly(t *testing.T, path string, pieces int, gap time.Duration) time.Time {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Error(err)
		return time.Time{}
	}
	for i := 0; i < pieces; i++ {
		if _, err := f.Write(make([]byte, 4096)); err != nil {
			t.Error(err)
		}
		time.Sleep(gap)
	}
	closed := time.Now()
	if err := f.Close(); err != nil {
		t.Error(err)
	}
	return closed
}

// A folder moved in while one of its files is still being written (cp -r into
// a staging folder, then mv, or a sync tool doing the same): the scan of the
// new folder must not take that file before it is complete.
func TestInotifyFolderMovedInWithAFileStillBeingWritten(t *testing.T) {
	fw, dir := startWatcher(t)
	staging := t.TempDir()
	album := filepath.Join(staging, "album")
	if err := os.Mkdir(album, 0o755); err != nil {
		t.Fatal(err)
	}
	done := filepath.Join(album, "done.jpg")
	if err := os.WriteFile(done, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Older than the settle time: a finished file.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(done, old, old); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "album")
	big := filepath.Join(moved, "big.mp4")
	closedAt := make(chan time.Time, 1)
	f, err := os.Create(filepath.Join(album, "big.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		// Keeps writing after the folder has moved: the open file goes
		// with it.
		for i := 0; i < 16; i++ {
			_, _ = f.Write(make([]byte, 4096))
			time.Sleep(fw.settle / 4)
		}
		closedAt <- time.Now()
		_ = f.Close()
	}()
	time.Sleep(fw.settle / 4)
	if err := os.Rename(album, moved); err != nil {
		t.Fatal(err)
	}
	got := collectTimed(fw, 4*fw.settle+3*time.Second)
	closed := <-closedAt
	if _, ok := got[filepath.Join(moved, "done.jpg")]; !ok {
		t.Errorf("the finished file in the moved folder was not taken: %v", got)
	}
	at, ok := got[big]
	if !ok {
		t.Fatalf("the file written into the moved folder was never taken: %v", got)
	}
	if at.Before(closed) {
		t.Errorf("the file was taken %s before it was complete", closed.Sub(at))
	}
}

// Files present at the start that are still being written are taken once
// they are complete, not before.
func TestInotifyStartupScanWaitsForFilesBeingWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "copying.mov")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := NewInotifyWatcher(dir, customlogger.NewWithLevel(nil, "error", ""), 8192)
	if err != nil {
		t.Fatal(err)
	}
	fw := w.(*inotifyWatcher)
	fw.settle = 200 * time.Millisecond
	if err := fw.Start(); err != nil {
		t.Fatal(err)
	}
	defer fw.Stop()
	closedAt := make(chan time.Time, 1)
	go func() {
		for i := 0; i < 12; i++ {
			_, _ = f.Write(make([]byte, 4096))
			time.Sleep(fw.settle / 4)
		}
		closedAt <- time.Now()
		_ = f.Close()
	}()
	got := collectTimed(fw, 3*time.Second)
	closed := <-closedAt
	at, ok := got[path]
	if !ok {
		t.Fatalf("never taken: %v", got)
	}
	if at.Before(closed) {
		t.Errorf("taken %s before it was complete", closed.Sub(at))
	}
}

// Some tools write under a temporary name and publish the file with a hard
// link (and then remove the temporary name), instead of a rename.
func TestInotifyHardLinkPublishing(t *testing.T) {
	fw, dir := startWatcher(t)
	tmp := filepath.Join(dir, "IMG_1.jpg.tmp")
	final := filepath.Join(dir, "IMG_1.jpg")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(tmp, final); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	got := collect(fw, 2*time.Second)
	if !got[final] {
		t.Errorf("the linked file was not taken: %v", got)
	}
	if got[tmp] {
		t.Error("the temporary name was taken")
	}
}

// What is noted about a temporary file's inode must not make a later file
// with the same inode number be taken as soon as it is created. The kernel
// reuses inode numbers quickly, which is what this provokes.
func TestInotifyReusedInodeIsNotTakenEarly(t *testing.T) {
	fw, dir := startWatcher(t)
	for round := 0; round < 20; round++ {
		tmp := filepath.Join(dir, fmt.Sprintf("a%d.jpg.part", round))
		final := filepath.Join(dir, fmt.Sprintf("a%d.jpg", round))
		if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, final); err != nil {
			t.Fatal(err)
		}
		collect(fw, 50*time.Millisecond)
		if err := os.Remove(final); err != nil {
			t.Fatal(err)
		}
	}
	slow := filepath.Join(dir, "slow.mov")
	closedAt := make(chan time.Time, 1)
	go func() { closedAt <- writeSlowly(t, slow, 10, 30*time.Millisecond) }()
	got := collectTimed(fw, 2*time.Second)
	closed := <-closedAt
	at, ok := got[slow]
	if !ok {
		t.Fatalf("never taken: %v", got)
	}
	if at.Before(closed) {
		t.Errorf("a new file was taken %s before it was complete (a stale note about its inode)", closed.Sub(at))
	}
}

// The same, in the order that loses the file on a busy machine: the
// temporary name is already gone when its close event is handled, so its
// inode cannot be learnt, and the link's creation looks like a file that
// is still being written. It is taken once it has settled.
func TestInotifyHardLinkWhoseTemporaryNameIsGoneFirst(t *testing.T) {
	fw, _ := startWatcher(t)
	elsewhere := t.TempDir() // not watched: the events are handed over below
	final := filepath.Join(elsewhere, "IMG_2.jpg")
	if err := os.WriteFile(final, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(final, old, old); err != nil {
		t.Fatal(err)
	}
	fw.handleInotifyEvent(&unix.InotifyEvent{Mask: unix.IN_CLOSE_WRITE}, "IMG_2.jpg.tmp", elsewhere)
	fw.handleInotifyEvent(&unix.InotifyEvent{Mask: unix.IN_CREATE}, "IMG_2.jpg", elsewhere)
	got := collect(fw, 4*fw.settle)
	if !got[final] {
		t.Errorf("the linked file was not taken: %v", got)
	}
}

// An SFTP client that keeps the capture time (seen with an Android sync app
// and its .tacitpart files) closes the temporary file, sets its mtime back
// to when the photo was taken, then publishes it with link and unlink. The
// close is handled while the temporary name still exists, but the link's
// creation is handled after the unlink, so the inode has one link and an
// mtime that no longer matches what was noted at the close. It is still the
// same complete file and must be taken.
func TestInotifyHardLinkAfterTheMtimeWasSetBack(t *testing.T) {
	fw, _ := startWatcher(t)
	elsewhere := t.TempDir() // not watched: the events are handed over below
	tmp := filepath.Join(elsewhere, "IMG_3.jpg_0b7e_.tacitpart")
	final := filepath.Join(elsewhere, "IMG_3.jpg")
	if err := os.WriteFile(tmp, []byte("photo"), 0o644); err != nil {
		t.Fatal(err)
	}
	fw.handleInotifyEvent(&unix.InotifyEvent{Mask: unix.IN_CLOSE_WRITE}, filepath.Base(tmp), elsewhere)
	taken := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(tmp, taken, taken); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(tmp, final); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	fw.handleInotifyEvent(&unix.InotifyEvent{Mask: unix.IN_CREATE}, filepath.Base(final), elsewhere)
	got := collect(fw, 4*fw.settle)
	if !got[final] {
		t.Errorf("the linked file was not taken: %v", got)
	}
}

// A file that is still open is not taken because its writer paused: it may
// be an upload over a network share that stalled. Its close announces it.
func TestInotifyOpenFileIsNotTakenWhileTheWriterPauses(t *testing.T) {
	fw, dir := startWatcher(t)
	// Past the startup scan, which takes whatever looks settled: files
	// present at start have no events to wait for.
	collect(fw, fw.settle)
	path := filepath.Join(dir, "VID_1.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("first half")); err != nil {
		t.Fatal(err)
	}
	// Looks long untouched, as after a stall longer than the settle time.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if got := collect(fw, 4*fw.settle); got[path] {
		t.Fatal("taken while still open")
	}
	if _, err := f.Write([]byte(" and the rest")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := collect(fw, 2*time.Second); !got[path] {
		t.Errorf("not taken once closed: %v", got)
	}
}
