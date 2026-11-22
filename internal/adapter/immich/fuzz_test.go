package immich

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// discardConn is a connection whose writes go nowhere.
type discardConn struct{ net.Conn }

func (discardConn) Write(b []byte) (int, error)      { return len(b), nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }
func (discardConn) SetDeadline(time.Time) error      { return nil }
func (discardConn) SetReadDeadline(time.Time) error  { return nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (discardConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }

// What Immich's websocket sends is read frame by frame. Whatever arrives, the
// reader must end (with a message or an error) without crashing, and without
// taking more memory than a message may have.
func FuzzEventFrames(f *testing.F) {
	frame := func(op byte, fin bool, payload string) []byte {
		b := []byte{op, byte(len(payload))}
		if fin {
			b[0] |= 0x80
		}
		return append(b, payload...)
	}
	f.Add(frame(0x1, true, `42["on_upload_success",{"id":"a","checksum":"b"}]`))
	f.Add(append(frame(0x9, true, "p"), frame(0x1, true, "2")...))
	f.Add(append(frame(0x1, false, "4"), frame(0x0, true, "0")...))
	f.Add([]byte{0x81, 127, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		w := &wsConn{conn: discardConn{}, r: bufio.NewReader(bytes.NewReader(data))}
		for i := 0; i < 64; i++ {
			msg, err := w.read()
			if err != nil {
				return
			}
			// What the message says is acted on: neither may crash.
			_ = (&EventClient{}).handle(w, msg, handlers())
		}
	})
}

func handlers() port.LibraryEventHandlers {
	return port.LibraryEventHandlers{
		Connected:    func() {},
		AssetUpdated: func(string, []string) {},
		Uploaded:     func(string, string) {},
	}
}

func FuzzDispatch(f *testing.F) {
	f.Add(`["on_asset_update",{"id":"a","tags":[{"value":"immich-optimizer/convert"}]}]`)
	f.Add(`["on_upload_success",{"id":"a","checksum":"b"}]`)
	f.Add(`["on_upload_success"]`)
	f.Fuzz(func(t *testing.T, payload string) {
		dispatch(payload, handlers())
	})
}
