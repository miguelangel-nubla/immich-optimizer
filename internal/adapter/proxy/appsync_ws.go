package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/immich"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// The app does not wait for its next sync to learn about an asset it just
// uploaded: Immich tells it over the websocket ("AssetUploadReady") as soon
// as the asset is ready. That message has no marker in it, so the hidden
// entry would only arrive with the next sync and the app would show the photo
// as still to be backed up until then.
//
// So, with app sync on, the proxy carries the websocket itself. Everything
// passes through untouched in both directions; after an "AssetUploadReady"
// message for an asset that has a marker, the proxy adds a second message of
// the same kind for the hidden entry.

const (
	socketPath = "/api/socket.io/"
	// maxInspectedFrame bounds the messages that are read into memory to
	// look at them. Bigger ones are passed through unread.
	maxInspectedFrame = 1 << 20
)

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func isWebsocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet && headerHasToken(r.Header, "Connection", "upgrade") &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func (s *Server) dialUpstream(ctx context.Context) (net.Conn, error) {
	u := s.cfg.UpstreamURL
	host := u.Host
	d := &net.Dialer{Timeout: 30 * time.Second}
	if u.Scheme == "https" {
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "443")
		}
		return (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", host)
	}
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	return d.DialContext(ctx, "tcp", host)
}

// wsHandshakeTimeout is how long Immich may take to answer the websocket
// handshake.
const wsHandshakeTimeout = 30 * time.Second

// serveSocket carries the app's websocket. It reports whether it answered the
// request; when it did not, nothing was written.
func (s *Server) serveSocket(w http.ResponseWriter, r *http.Request) bool {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return false
	}
	up, err := s.dialUpstream(r.Context())
	if err != nil {
		s.logger.Errorf("Websocket: could not reach Immich: %v", err)
		http.Error(w, "failed to reach Immich", http.StatusBadGateway)
		return true
	}
	defer func() { _ = up.Close() }()

	// Immich has to answer the handshake in reasonable time; without a limit
	// a server that accepts and then says nothing would hold this connection
	// and the app's for good.
	_ = up.SetDeadline(time.Now().Add(wsHandshakeTimeout))

	out := r.Clone(r.Context())
	out.Host = s.cfg.UpstreamURL.Host
	// No compression: the messages have to be readable here.
	out.Header.Del("Sec-WebSocket-Extensions")
	if err := out.Write(up); err != nil {
		http.Error(w, "failed to reach Immich", http.StatusBadGateway)
		return true
	}
	upR := bufio.NewReader(up)
	resp, err := http.ReadResponse(upR, out)
	if err != nil {
		http.Error(w, "failed to reach Immich", http.StatusBadGateway)
		return true
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			w.Header()[k] = vv
		}
		for _, k := range hopHeaders {
			w.Header().Del(k)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return true
	}

	_ = up.SetDeadline(time.Time{})

	client, crw, err := hj.Hijack()
	if err != nil {
		s.logger.Errorf("Websocket: could not take over the connection: %v", err)
		return true
	}
	defer func() { _ = client.Close() }()
	if err := resp.Write(client); err != nil {
		return true
	}

	// The connection outlives the request: end it when the proxy stops.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.baseCtx.Done():
			_ = client.Close()
			_ = up.Close()
		case <-done:
		}
	}()
	go func() {
		_, _ = io.Copy(up, crw.Reader)
		_ = up.Close()
	}()

	auth := immich.AuthHeaders(r.Header)
	pump := &socketPump{hidden: func(payload []byte) []byte {
		return s.hiddenUploadReady(s.baseCtx, auth, payload)
	}}
	_ = pump.run(client, upR)
	return true
}

// socketPump copies websocket frames from the server to the client and adds
// a message after the ones hidden answers to.
type socketPump struct {
	// hidden returns the text of the message to add after one, or nil.
	hidden func(payload []byte) []byte
}

func textFrame(payload []byte) []byte {
	n := len(payload)
	var f []byte
	switch {
	case n < 126:
		f = []byte{0x81, byte(n)}
	case n <= 0xffff:
		f = []byte{0x81, 126, 0, 0}
		binary.BigEndian.PutUint16(f[2:], uint16(n))
	default:
		f = make([]byte, 10)
		f[0], f[1] = 0x81, 127
		binary.BigEndian.PutUint64(f[2:], uint64(n))
	}
	return append(f, payload...)
}

func (p *socketPump) run(dst io.Writer, src *bufio.Reader) error {
	for {
		head := make([]byte, 2, 14)
		if _, err := io.ReadFull(src, head); err != nil {
			return err
		}
		length := uint64(head[1] & 0x7f)
		more := 0
		switch length {
		case 126:
			more = 2
		case 127:
			more = 8
		}
		masked := head[1]&0x80 != 0
		if masked {
			more += 4
		}
		head = head[:2+more]
		if _, err := io.ReadFull(src, head[2:]); err != nil {
			return err
		}
		switch head[1] & 0x7f {
		case 126:
			length = uint64(binary.BigEndian.Uint16(head[2:4]))
		case 127:
			length = binary.BigEndian.Uint64(head[2:10])
		}

		// Only a complete, plain text message is looked at. A new message
		// may be added right after it: no other message can be under way.
		whole := head[0] == 0x81 && !masked
		if !whole || length > maxInspectedFrame {
			if _, err := dst.Write(head); err != nil {
				return err
			}
			if _, err := io.CopyN(dst, src, int64(length)); err != nil { // #nosec G115 -- a frame length is below 2^63
				return err
			}
			continue
		}
		frame := make([]byte, len(head)+int(length)) // #nosec G115 -- bounded by maxInspectedFrame
		copy(frame, head)
		if _, err := io.ReadFull(src, frame[len(head):]); err != nil {
			return err
		}
		if _, err := dst.Write(frame); err != nil {
			return err
		}
		if extra := p.hidden(frame[len(head):]); extra != nil {
			if _, err := dst.Write(textFrame(extra)); err != nil {
				return err
			}
		}
	}
}

// socketEvent is the prefix of a socket.io event message on the default
// namespace.
var socketEvent = []byte("42[")

// hiddenUploadReady returns the message to send after an "AssetUploadReady"
// message: the same one for the hidden entry of that asset. It returns nil
// for every other message and for assets without a marker.
func (s *Server) hiddenUploadReady(ctx context.Context, auth http.Header, payload []byte) []byte {
	if !bytes.HasPrefix(payload, socketEvent) || !bytes.Contains(payload[:min(len(payload), 64)], []byte(`"AssetUploadReadyV`)) {
		return nil
	}
	var msg []json.RawMessage
	if json.Unmarshal(payload[2:], &msg) != nil || len(msg) != 2 {
		return nil
	}
	var name string
	if json.Unmarshal(msg[0], &name) != nil || (name != "AssetUploadReadyV1" && name != "AssetUploadReadyV2") {
		return nil
	}
	var data map[string]json.RawMessage
	var asset, exif map[string]any
	if json.Unmarshal(msg[1], &data) != nil || json.Unmarshal(data["asset"], &asset) != nil || json.Unmarshal(data["exif"], &exif) != nil ||
		asset == nil || exif == nil {
		return nil
	}
	assetID, _ := asset["id"].(string)
	owner, _ := asset["ownerId"].(string)
	id := hiddenID(assetID)
	if id == "" || owner == "" {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	m, err := immich.GetMarker(ctx, s.client, s.cfg.UpstreamURL.String(), auth, assetID)
	if err != nil {
		s.logger.Warnf("App sync: could not read the marker of asset %s: %v", assetID, err)
		return nil
	}
	sum, ok := hiddenChecksum(m)
	if !ok {
		return nil
	}
	// The real asset's own fields, with the ones that make it the hidden
	// entry replaced: whatever else this version of the message carries
	// stays as the app expects it.
	for k, v := range hiddenAsset(id, owner, sum) {
		asset[k] = v
	}
	exif["assetId"] = id
	out, err := json.Marshal([]any{name, map[string]any{"asset": asset, "exif": exif}})
	if err != nil {
		return nil
	}
	s.logger.Debugf("App sync: added the hidden entry of asset %s to the websocket", assetID)
	return append([]byte("42"), out...)
}

// hiddenChecksum returns the checksum a hidden entry gets from a marker, or
// false when the marker's asset gets no hidden entry: an original that was
// kept as it is (its own checksum is already right) or a replacement that is
// not finished.
func hiddenChecksum(m *entity.Marker) ([]byte, bool) {
	if m == nil || m.Kept || m.State == entity.MarkerStatePending {
		return nil, false
	}
	sum, err := hex.DecodeString(m.SHA1)
	if err != nil || len(sum) != 20 {
		return nil, false
	}
	return sum, true
}
