package proxy

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// jobSpec is everything a detached job needs from the original request. It
// deliberately holds no *http.Request and no request context.
type jobSpec struct {
	method     string
	url        *url.URL
	header     http.Header
	remoteAddr string
	userAgent  string
	spoolPath  string
}

func newJobSpec(r *http.Request, spoolPath string) jobSpec {
	u := *r.URL
	return jobSpec{
		method:     r.Method,
		url:        &u,
		header:     r.Header.Clone(),
		remoteAddr: r.RemoteAddr,
		userAgent:  r.UserAgent(),
		spoolPath:  spoolPath,
	}
}

type job struct {
	id    string
	key   string // coalescing key; "" when coalescing is disabled
	async bool

	done       chan struct{} // closed once by finish
	result     jobResult     // read only after done is closed
	finishedAt time.Time     // guarded by jobManager.mu
}

func (j *job) isDone() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

type jobManager struct {
	mu      sync.Mutex
	byID    map[string]*job // async jobs, for /wait lookups
	byKey   map[string]*job // in-flight and recently finished jobs, for coalescing
	ttl     time.Duration
	maxKept int
	now     func() time.Time
}

func newJobManager(ttl time.Duration, maxKept int) *jobManager {
	return &jobManager{
		byID:    make(map[string]*job),
		byKey:   make(map[string]*job),
		ttl:     ttl,
		maxKept: maxKept,
		now:     time.Now,
	}
}

func (m *jobManager) create(async bool) *job {
	j := &job{id: generateUUID(), async: async, done: make(chan struct{})}
	if async {
		m.mu.Lock()
		m.byID[j.id] = j
		m.mu.Unlock()
	}
	return j
}

func (m *jobManager) expiredLocked(j *job) bool {
	return !j.finishedAt.IsZero() && m.now().Sub(j.finishedAt) > m.ttl
}

func (m *jobManager) get(id string) *job {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.byID[id]
	if j == nil {
		return nil
	}
	if m.expiredLocked(j) {
		delete(m.byID, id)
		return nil
	}
	return j
}

// attach registers self under key, or returns an existing in-flight or
// unexpired finished job with the same key.
func (m *jobManager) attach(key string, self *job) (*job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing := m.byKey[key]; existing != nil && existing != self && !m.expiredLocked(existing) {
		return existing, true
	}
	self.key = key
	m.byKey[key] = self
	return nil, false
}

func (m *jobManager) finish(j *job, res jobResult) {
	m.mu.Lock()
	j.result = res
	j.finishedAt = m.now()
	if j.key != "" && m.byKey[j.key] == j && !res.cacheable {
		delete(m.byKey, j.key)
	}
	close(j.done)
	m.evictLocked()
	m.mu.Unlock()
}

// evictLocked drops the oldest finished jobs beyond maxKept.
func (m *jobManager) evictLocked() {
	for len(m.byID)+len(m.byKey) > m.maxKept {
		var oldest *job
		for _, mp := range []map[string]*job{m.byID, m.byKey} {
			for _, j := range mp {
				if !j.finishedAt.IsZero() && (oldest == nil || j.finishedAt.Before(oldest.finishedAt)) {
					oldest = j
				}
			}
		}
		if oldest == nil {
			return
		}
		m.removeLocked(oldest)
	}
}

func (m *jobManager) removeLocked(j *job) {
	if m.byID[j.id] == j {
		delete(m.byID, j.id)
	}
	if j.key != "" && m.byKey[j.key] == j {
		delete(m.byKey, j.key)
	}
}

func (m *jobManager) sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mp := range []map[string]*job{m.byID, m.byKey} {
		for _, j := range mp {
			if m.expiredLocked(j) {
				m.removeLocked(j)
			}
		}
	}
}

// janitor drops expired results every minute until ctx is done.
func (m *jobManager) janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.sweep()
		}
	}
}

func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
