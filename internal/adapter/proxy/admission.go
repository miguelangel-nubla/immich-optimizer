package proxy

import (
	"context"
	"sync"
	"time"
)

// Uploads arrive as fast as the network carries them and leave as fast as
// they can be converted. Every upload that is waiting or being converted
// has files in the temp folder, so a client that sends faster than the tasks
// convert would fill the disk: the conversions under way then fail for lack
// of room, although only the newest upload needed to wait.
//
// admission makes that upload wait. Before the body of an upload is read, it
// checks that the temp folder has room for it next to what the uploads
// already taken on may still write. There is no setting: the limit is the
// free space itself.

const (
	// copiesPerUpload is how many times its own size an upload can take in
	// the temp folder: the request as received, the file taken out of it,
	// and the converted result (which is kept only when it is smaller).
	copiesPerUpload = 3
	// admissionWait is how long an upload is held for room before it is
	// turned away. Reverse proxies in front commonly give up after a minute.
	admissionWait = 45 * time.Second
	// admissionRetryAfter is the Retry-After of an upload turned away.
	admissionRetryAfter = "30"
)

type admission struct {
	// free returns the free space of the temp folder in bytes, or false
	// when it cannot be told; then nothing is held back.
	free func() (int64, bool)
	wait time.Duration

	mu       sync.Mutex
	count    int           // uploads taken on and not finished
	bytes    int64         // their request sizes
	released chan struct{} // closed, and replaced, when one finishes
}

func newAdmission(free func() (int64, bool)) *admission {
	return &admission{free: free, wait: admissionWait, released: make(chan struct{})}
}

// ticket is one upload's place. Release it when its files are gone.
type ticket struct {
	a    *admission
	size int64
	once sync.Once
}

func (t *ticket) release() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		a := t.a
		a.mu.Lock()
		a.count--
		a.bytes -= t.size
		close(a.released)
		a.released = make(chan struct{})
		a.mu.Unlock()
	})
}

// resize corrects the ticket once the real size of the request is known.
func (t *ticket) resize(size int64) {
	t.a.mu.Lock()
	t.a.bytes += size - t.size
	t.size = size
	t.a.mu.Unlock()
}

// fitsLocked reports whether an upload of size bytes can be taken on now.
func (a *admission) fitsLocked(size int64) bool {
	// With nothing else going on, waiting would not make room: let it try.
	if a.count == 0 {
		return true
	}
	free, ok := a.free()
	if !ok {
		return true
	}
	// The uploads taken on have written their request already and may
	// still write the rest of their copies.
	return free-(copiesPerUpload-1)*a.bytes >= copiesPerUpload*size
}

// acquire takes on an upload whose request is size bytes long (0 when the
// client did not say). It waits for room for a while, and returns nil when
// there is still none, or ctx ends.
func (a *admission) acquire(ctx context.Context, size int64) *ticket {
	if size < 0 {
		size = 0
	}
	deadline := time.NewTimer(a.wait)
	defer deadline.Stop()
	// Room is also made by things this cannot see (another program's files
	// going away), so it looks again now and then.
	again := time.NewTicker(time.Second)
	defer again.Stop()
	for {
		a.mu.Lock()
		if a.fitsLocked(size) {
			a.count++
			a.bytes += size
			a.mu.Unlock()
			return &ticket{a: a, size: size}
		}
		released := a.released
		a.mu.Unlock()
		select {
		case <-released:
		case <-again.C:
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}
