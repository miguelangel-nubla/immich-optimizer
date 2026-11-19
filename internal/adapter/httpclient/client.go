// Package httpclient builds the HTTP client used for uploads to Immich.
//
// It deliberately has no http.Client.Timeout: that would cap the whole
// exchange, including sending the request body, and kill large uploads.
// Instead the transport bounds connecting, the TLS handshake and the wait for
// response headers after the body has been sent, and a watchdog aborts an
// upload whose body makes no progress for StallTimeout (an upstream that
// stops reading would otherwise hang the upload forever).
package httpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	DialTimeout           time.Duration // default 30s
	TLSHandshakeTimeout   time.Duration // default 15s
	ResponseHeaderTimeout time.Duration // default 120s; 0 in Options means default
	// StallTimeout aborts a request whose body is not read for this long
	// once sending has started. Default 120s.
	StallTimeout time.Duration
}

// ErrUploadStalled is returned (wrapped) when a request body made no
// progress for StallTimeout.
var ErrUploadStalled = errors.New("upload stalled: request body made no progress")

const (
	DefaultDialTimeout           = 30 * time.Second
	DefaultTLSHandshakeTimeout   = 15 * time.Second
	DefaultResponseHeaderTimeout = 120 * time.Second
	DefaultStallTimeout          = 120 * time.Second
)

func New(o Options) *http.Client {
	if o.DialTimeout <= 0 {
		o.DialTimeout = DefaultDialTimeout
	}
	if o.TLSHandshakeTimeout <= 0 {
		o.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	if o.ResponseHeaderTimeout <= 0 {
		o.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
	}
	if o.StallTimeout <= 0 {
		o.StallTimeout = DefaultStallTimeout
	}

	var tr *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = dt.Clone()
	} else {
		tr = &http.Transport{}
	}
	dialer := &net.Dialer{Timeout: o.DialTimeout, KeepAlive: 30 * time.Second}
	tr.Proxy = http.ProxyFromEnvironment
	tr.DialContext = dialer.DialContext
	tr.TLSHandshakeTimeout = o.TLSHandshakeTimeout
	tr.ResponseHeaderTimeout = o.ResponseHeaderTimeout
	tr.ExpectContinueTimeout = 1 * time.Second
	tr.IdleConnTimeout = 90 * time.Second

	return &http.Client{Transport: &stallTransport{next: tr, idle: o.StallTimeout}, Timeout: 0}
}

// stallTransport cancels a request whose body stops being consumed.
type stallTransport struct {
	next http.RoundTripper
	idle time.Duration
}

func (t *stallTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil || req.Body == http.NoBody || t.idle <= 0 {
		return t.next.RoundTrip(req)
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	body := &progressBody{rc: req.Body}
	r2 := req.WithContext(ctx)
	r2.Body = body

	stop := make(chan struct{})
	var stopOnce sync.Once
	stopWatch := func() { stopOnce.Do(func() { close(stop) }) }
	go body.watch(t.idle, stop, cancel)

	resp, err := t.next.RoundTrip(r2)
	stopWatch()
	if err != nil {
		cause := context.Cause(ctx)
		cancel(nil)
		if errors.Is(cause, ErrUploadStalled) {
			return nil, errors.Join(ErrUploadStalled, err)
		}
		return nil, err
	}
	// Keep ctx alive while the caller reads the response body.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

type progressBody struct {
	rc   io.ReadCloser
	last atomic.Int64 // unix nanos of the last read; 0 until sending starts
	done atomic.Bool  // body fully read or closed
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.last.Store(time.Now().UnixNano())
	if err != nil {
		b.done.Store(true)
	}
	return n, err
}

func (b *progressBody) Close() error {
	b.done.Store(true)
	return b.rc.Close()
}

func (b *progressBody) watch(idle time.Duration, stop <-chan struct{}, cancel context.CancelCauseFunc) {
	tick := idle / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if b.done.Load() {
				return // the response header timeout takes over
			}
			last := b.last.Load()
			if last != 0 && time.Since(time.Unix(0, last)) > idle {
				cancel(ErrUploadStalled)
				return
			}
		}
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel func()
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
