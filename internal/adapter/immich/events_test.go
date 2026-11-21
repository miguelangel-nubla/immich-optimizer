package immich

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// fakeSocketServer speaks just enough socket.io to stand in for Immich.
type fakeSocketServer struct {
	t      *testing.T
	key    string
	conns  chan *serverConn
	server *httptest.Server
}

type serverConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// send writes an unmasked text frame, as a server does.
func (c *serverConn) send(t *testing.T, text string) {
	t.Helper()
	p := []byte(text)
	hdr := []byte{0x81}
	if len(p) < 126 {
		hdr = append(hdr, byte(len(p)))
	} else {
		hdr = append(hdr, 126, byte(len(p)>>8), byte(len(p)))
	}
	if _, err := c.conn.Write(append(hdr, p...)); err != nil {
		t.Fatal(err)
	}
}

// recv reads one masked frame from the client.
func (c *serverConn) recv(t *testing.T) (byte, string) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		t.Fatal(err)
	}
	if hdr[1]&0x80 == 0 {
		t.Fatal("client frame is not masked")
	}
	n := int(hdr[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		_, _ = io.ReadFull(c.r, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	var mask [4]byte
	_, _ = io.ReadFull(c.r, mask[:])
	p := make([]byte, n)
	_, _ = io.ReadFull(c.r, p)
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return hdr[0] & 0x0F, string(p)
}

func newFakeSocketServer(t *testing.T) *fakeSocketServer {
	s := &fakeSocketServer{t: t, key: "the-api-key", conns: make(chan *serverConn, 4)}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/socket.io/" || r.URL.Query().Get("EIO") != "4" || r.Header.Get("x-api-key") != s.key ||
			r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		s.conns <- &serverConn{conn: conn, r: rw.Reader}
	}))
	t.Cleanup(s.server.Close)
	return s
}

// accept waits for the client and completes the socket.io handshake.
func (s *fakeSocketServer) accept() *serverConn {
	s.t.Helper()
	select {
	case c := <-s.conns:
		c.send(s.t, `0{"sid":"x","upgrades":[],"pingInterval":25000,"pingTimeout":20000}`)
		if _, msg := c.recv(s.t); msg != "40" {
			s.t.Fatalf("client sent %q, want 40", msg)
		}
		c.send(s.t, `40{"sid":"y"}`)
		return c
	case <-time.After(5 * time.Second):
		s.t.Fatal("the client did not connect")
		return nil
	}
}

func TestEventClient(t *testing.T) {
	srv := newFakeSocketServer(t)
	var mu sync.Mutex
	var connected int
	var updated, uploaded []string
	h := port.LibraryEventHandlers{
		Connected: func() { mu.Lock(); connected++; mu.Unlock() },
		AssetUpdated: func(id string, tags []string) {
			mu.Lock()
			updated = append(updated, id+":"+join(tags))
			mu.Unlock()
		},
		Uploaded: func(id, sum string) { mu.Lock(); uploaded = append(uploaded, id+":"+sum); mu.Unlock() },
	}
	c := NewEventClient(srv.server.URL, srv.key)
	c.MaxBackoff = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Listen(ctx, h); close(done) }()
	defer func() { cancel(); <-done }()

	conn := srv.accept()
	// Immich's ping is answered.
	conn.send(t, "2")
	if _, msg := conn.recv(t); msg != "3" {
		t.Fatalf("ping answered with %q", msg)
	}
	// A websocket ping too.
	_, _ = conn.conn.Write([]byte{0x89, 0})
	if op, _ := conn.recv(t); op != 0xA {
		t.Fatalf("websocket ping answered with opcode %x", op)
	}
	conn.send(t, `42["on_asset_update",{"id":"a1","tags":[{"value":"immich-optimizer/convert"},{"value":"holiday"}]}]`)
	conn.send(t, `42["on_upload_success",{"id":"a2","checksum":"bqU/h7UB7XRNJid2FxAZFwwy5HI="}]`)
	conn.send(t, `42["on_server_version",{"major":3}]`)
	conn.send(t, `42["on_asset_update","not an asset"]`)
	// A long message, as a full asset is.
	long := `42["on_asset_update",{"id":"a3","description":"` + strings.Repeat("x", 300) + `","tags":[]}]`
	conn.send(t, long)
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(updated) == 2 && len(uploaded) == 1 })
	mu.Lock()
	if updated[0] != "a1:immich-optimizer/convert,holiday" || updated[1] != "a3:" || uploaded[0] != "a2:bqU/h7UB7XRNJid2FxAZFwwy5HI=" {
		t.Errorf("updated %v, uploaded %v", updated, uploaded)
	}
	mu.Unlock()

	// A lost connection is made again, and that is reported.
	_ = conn.conn.Close()
	srv.accept()
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return connected == 2 })
}

func TestEventClientRejectedKeyRetries(t *testing.T) {
	srv := newFakeSocketServer(t)
	errs := make(chan string, 10)
	c := NewEventClient(srv.server.URL, "wrong-key")
	c.MaxBackoff = 10 * time.Millisecond
	c.Errorf = func(format string, args ...any) { errs <- format }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Listen(ctx, port.LibraryEventHandlers{}); close(done) }()
	for i := 0; i < 2; i++ {
		select {
		case <-errs:
		case <-time.After(5 * time.Second):
			t.Fatal("a refused connection was not reported")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not end with its context")
	}
}

func join(v []string) string {
	out := ""
	for i, s := range v {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func waitUntil(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
