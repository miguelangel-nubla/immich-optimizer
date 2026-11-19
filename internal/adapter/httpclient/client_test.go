package httpclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewHasNoWholeRequestTimeout(t *testing.T) {
	c := New(Options{DialTimeout: time.Second, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 3 * time.Second})
	if c.Timeout != 0 {
		t.Fatalf("http.Client.Timeout must be 0, got %s", c.Timeout)
	}
	tr := c.Transport.(*stallTransport).next.(*http.Transport)
	if tr.TLSHandshakeTimeout != 2*time.Second || tr.ResponseHeaderTimeout != 3*time.Second || tr.DialContext == nil || tr.Proxy == nil {
		t.Fatalf("transport timeouts not set: %+v", tr)
	}
	d := New(Options{}).Transport.(*stallTransport).next.(*http.Transport)
	if d.ResponseHeaderTimeout != DefaultResponseHeaderTimeout || d.TLSHandshakeTimeout != DefaultTLSHandshakeTimeout {
		t.Fatalf("defaults not applied")
	}
}

// slowReader yields data in small chunks with delays, like a large upload on
// a slow link.
type slowReader struct {
	n     int
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.n == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.delay)
	s.n--
	return copy(p, bytes.Repeat([]byte("x"), 10)), nil
}

func TestSlowBodyNotKilledByHeaderTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := New(Options{ResponseHeaderTimeout: 500 * time.Millisecond})
	// ~2s to send the body; the header timeout only starts after the body.
	req, _ := http.NewRequest(http.MethodPost, srv.URL, &slowReader{n: 10, delay: 200 * time.Millisecond})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("slow upload was killed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestSlowResponseHeadersTimeOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)
	c := New(Options{ResponseHeaderTimeout: 200 * time.Millisecond})
	resp, err := c.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("expected a response header timeout")
	}
}

// stuckTransport reads one chunk of the body and then stops reading, like an
// upstream whose TCP window never opens again.
type stuckTransport struct{}

func (stuckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	buf := make([]byte, 4)
	if _, err := req.Body.Read(buf); err != nil {
		return nil, err
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestStalledUploadIsAborted(t *testing.T) {
	c := &http.Client{Transport: &stallTransport{next: stuckTransport{}, idle: 100 * time.Millisecond}}
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/", bytes.NewReader(make([]byte, 1024)))
	done := make(chan error, 1)
	go func() {
		resp, err := c.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUploadStalled) {
			t.Fatalf("expected ErrUploadStalled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("stalled upload was never aborted")
	}
}

func TestSlowButProgressingUploadNotStalled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	c := New(Options{StallTimeout: 150 * time.Millisecond})
	// 1s total, but never more than 50ms without progress.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, &slowReader{n: 20, delay: 50 * time.Millisecond})
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("progressing upload was aborted: %v", err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != "ok" {
		t.Fatalf("response body: %q %v", b, err)
	}
}
