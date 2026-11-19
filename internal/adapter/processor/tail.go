package processor

import (
	"sync"
	"unicode/utf8"
)

// tailBuffer is an io.Writer that keeps only the last max bytes written, so a
// chatty task (e.g. HandBrake progress output) cannot grow memory unbounded.
type tailBuffer struct {
	mu        sync.Mutex
	max       int
	buf       []byte
	truncated bool
}

func newTailBuffer(max int) *tailBuffer {
	return &tailBuffer{max: max}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		t.truncated = true
		return n, nil
	}
	t.buf = append(t.buf, p...)
	// Trim lazily (at 2x) to keep the amortised cost of writes low.
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
		t.truncated = true
	}
	return n, nil
}

// Tail returns at most n trailing bytes, starting at a UTF-8 rune boundary.
func (t *tailBuffer) Tail(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	if len(b) > t.max {
		b = b[len(b)-t.max:]
	}
	if len(b) > n {
		b = b[len(b)-n:]
		for i := 0; i < len(b) && i < utf8.UTFMax && !utf8.RuneStart(b[0]); i++ {
			b = b[1:]
		}
	}
	return string(b)
}

// Len returns the number of bytes currently retained (bounded by 2*max).
func (t *tailBuffer) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buf)
}
