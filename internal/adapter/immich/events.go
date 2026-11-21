package immich

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// EventClient listens to Immich's websocket as the API key's user, the way
// the web and mobile apps do. Immich speaks socket.io (engine.io v4) over a
// plain websocket; this is the small part of both that receiving events
// needs.
type EventClient struct {
	baseURL string
	apiKey  string
	// TLS is used for https addresses. Nil means the system's defaults.
	TLS *tls.Config
	// Errorf reports a lost connection. Optional.
	Errorf func(format string, args ...any)
	// MaxBackoff caps the wait between reconnection attempts.
	MaxBackoff time.Duration
}

func NewEventClient(baseURL, apiKey string) *EventClient {
	return &EventClient{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, MaxBackoff: time.Minute}
}

// Listen implements port.LibraryEvents.
func (c *EventClient) Listen(ctx context.Context, h port.LibraryEventHandlers) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx, h)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // it was working: try again soon
		}
		if c.Errorf != nil {
			c.Errorf("Immich's event connection was lost (%v); reconnecting in %s", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > c.MaxBackoff {
			backoff = c.MaxBackoff
		}
	}
}

// session is one connection, from the handshake until it breaks.
func (c *EventClient) session(ctx context.Context, h port.LibraryEventHandlers) error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return err
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	if u.Scheme == "https" {
		cfg := c.TLS
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		cfg = cfg.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		conn, err = (&tls.Dialer{NetDialer: d, Config: cfg}).DialContext(ctx, "tcp", host)
	} else {
		conn, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()

	key := make([]byte, 16)
	_, _ = rand.Read(key)
	path := strings.TrimRight(u.Path, "/") + "/api/socket.io/?EIO=4&transport=websocket"
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\nSec-WebSocket-Version: 13\r\n" +
		"x-api-key: " + c.apiKey + "\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("handshake: Immich answered %s", resp.Status)
	}

	ws := &wsConn{conn: conn, r: r}
	for {
		// Immich pings every 25 seconds; a minute of silence is a dead
		// connection.
		_ = conn.SetReadDeadline(time.Now().Add(time.Minute))
		msg, err := ws.read()
		if err != nil {
			return err
		}
		if err := c.handle(ws, msg, h); err != nil {
			return err
		}
	}
}

// handle acts on one engine.io packet.
func (c *EventClient) handle(ws *wsConn, msg string, h port.LibraryEventHandlers) error {
	switch {
	case strings.HasPrefix(msg, "0"): // engine.io open: join the default namespace
		return ws.write("40")
	case msg == "2": // ping
		return ws.write("3")
	case strings.HasPrefix(msg, "40"): // connected to the namespace
		if h.Connected != nil {
			h.Connected()
		}
	case strings.HasPrefix(msg, "41"), strings.HasPrefix(msg, "44"):
		return fmt.Errorf("closed by Immich: %s", msg)
	case strings.HasPrefix(msg, "42"):
		dispatch(msg[2:], h)
	}
	return nil
}

// dispatch delivers a socket.io event: ["name", data].
func dispatch(payload string, h port.LibraryEventHandlers) {
	var ev []json.RawMessage
	if json.Unmarshal([]byte(payload), &ev) != nil || len(ev) < 2 {
		return
	}
	var name string
	if json.Unmarshal(ev[0], &name) != nil {
		return
	}
	var a struct {
		ID       string `json:"id"`
		Checksum string `json:"checksum"`
		Tags     []struct {
			Value string `json:"value"`
		} `json:"tags"`
	}
	switch name {
	case "on_asset_update":
		if h.AssetUpdated == nil || json.Unmarshal(ev[1], &a) != nil || a.ID == "" {
			return
		}
		tags := make([]string, 0, len(a.Tags))
		for _, t := range a.Tags {
			tags = append(tags, t.Value)
		}
		h.AssetUpdated(a.ID, tags)
	case "on_upload_success":
		if h.Uploaded == nil || json.Unmarshal(ev[1], &a) != nil || a.ID == "" {
			return
		}
		h.Uploaded(a.ID, a.Checksum)
	}
}

// wsConn is the client side of a websocket: frames out are masked, frames in
// are not.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

const (
	opContinuation = 0x0
	opText         = 0x1
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
	maxMessage     = 64 << 20
)

// read returns the next text message, answering control frames on the way.
func (w *wsConn) read() (string, error) {
	var msg []byte
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(w.r, hdr[:]); err != nil {
			return "", err
		}
		fin, op := hdr[0]&0x80 != 0, hdr[0]&0x0F
		n := uint64(hdr[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return "", err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.r, b[:]); err != nil {
				return "", err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > maxMessage || uint64(len(msg))+n > maxMessage {
			return "", errors.New("message too large")
		}
		var mask [4]byte
		masked := hdr[1]&0x80 != 0
		if masked {
			if _, err := io.ReadFull(w.r, mask[:]); err != nil {
				return "", err
			}
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(w.r, payload); err != nil {
			return "", err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case opPing:
			if err := w.frame(opPong, payload); err != nil {
				return "", err
			}
			continue
		case opPong:
			continue
		case opClose:
			return "", errors.New("closed by Immich")
		case opText, opContinuation:
			msg = append(msg, payload...)
			if fin {
				return string(msg), nil
			}
		default: // binary: socket.io does not use it for these events
			if fin {
				msg = msg[:0]
			}
		}
	}
}

func (w *wsConn) write(text string) error { return w.frame(opText, []byte(text)) }

func (w *wsConn) frame(op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	_, _ = rand.Read(mask[:])
	hdr = append(hdr, mask[:]...)
	out := make([]byte, len(payload))
	for i, b := range payload {
		out[i] = b ^ mask[i%4]
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err := w.conn.Write(append(hdr, out...))
	return err
}
