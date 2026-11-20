package proxy

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

// readFrame reads one unmasked websocket frame.
func readFrame(t *testing.T, r *bufio.Reader) (first byte, payload []byte) {
	t.Helper()
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		b := make([]byte, 2)
		_, _ = io.ReadFull(r, b)
		n = uint64(binary.BigEndian.Uint16(b))
	case 127:
		b := make([]byte, 8)
		_, _ = io.ReadFull(r, b)
		n = binary.BigEndian.Uint64(b)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	return head[0], payload
}

func uploadReady(name, assetID string) []byte {
	b, _ := json.Marshal([]any{name, map[string]any{
		"asset": map[string]any{"id": assetID, "ownerId": owner, "originalFileName": "photo.jxl", "checksum": "Y29udmVydGVk",
			"thumbhash": "dGh1bWI=", "visibility": "timeline", "libraryId": nil, "type": "IMAGE", "isFavorite": true,
			"fileCreatedAt": "2024-05-06T07:08:09.000Z", "somethingNew": 7},
		"exif": map[string]any{"assetId": assetID, "make": "Camera"},
	}})
	return append([]byte("42"), b...)
}

func TestPumpAddsAMessageOnlyAfterWholeTextMessages(t *testing.T) {
	var in bytes.Buffer
	big := bytes.Repeat([]byte("x"), 70000)
	in.Write(textFrame([]byte("42[\"hello\"]")))
	in.Write([]byte{0x01, 3, 'a', 'b', 'c'}) // first part of a split message
	in.Write([]byte{0x89, 1, 'p'})           // a ping in between
	in.Write([]byte{0x80, 2, 'd', 'e'})      // its last part
	in.Write([]byte{0x82, 2, 0, 1})          // binary
	in.Write(textFrame(big))                 // 64-bit length form is not needed, 16-bit is
	in.Write(textFrame([]byte("add")))
	want := append([]byte(nil), in.Bytes()...)
	want = append(want, textFrame([]byte("ADDED"))...)

	var seen []string
	p := &socketPump{hidden: func(b []byte) []byte {
		seen = append(seen, string(b[:min(len(b), 12)]))
		if string(b) == "add" {
			return []byte("ADDED")
		}
		return nil
	}}
	var out bytes.Buffer
	if err := p.run(&out, bufio.NewReader(&in)); err != io.EOF {
		t.Fatalf("run: %v", err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("the frames were changed on the way:\n got %d bytes\nwant %d bytes", out.Len(), len(want))
	}
	if len(seen) != 3 || seen[0] != `42["hello"]` || seen[2] != "add" {
		t.Errorf("messages looked at = %q", seen)
	}
}

func TestWebsocketThroughTheProxy(t *testing.T) {
	const kept = "3f908a71-185e-4fb6-960c-278a8d87e66b"
	const plain = "4f908a71-185e-4fb6-960c-278a8d87e66b"
	var gotExtensions string
	fromClient := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/assets/") {
			if r.Header.Get("Authorization") != "Bearer token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch {
			case strings.Contains(r.URL.Path, realAsset):
				_, _ = io.WriteString(w, `{"key":"immich-optimizer","value":{"v":1,"sha1":"`+abcHex+`"}}`)
			case strings.Contains(r.URL.Path, kept):
				_, _ = io.WriteString(w, `{"key":"immich-optimizer","value":{"v":1,"sha1":"`+abcHex+`","kept":true}}`)
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}
		if r.URL.Path != socketPath || r.URL.RawQuery != "EIO=4&transport=websocket" {
			t.Errorf("unexpected request %s", r.URL)
		}
		gotExtensions = r.Header.Get("Sec-WebSocket-Extensions")
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: x\r\n\r\n")
		_, _ = rw.Write(textFrame([]byte(`0{"sid":"abc"}`)))
		_, _ = rw.Write(textFrame(uploadReady("AssetUploadReadyV2", plain)))
		_, _ = rw.Write(textFrame(uploadReady("AssetUploadReadyV2", kept)))
		_, _ = rw.Write(textFrame(uploadReady("AssetUploadReadyV2", realAsset)))
		_, _ = rw.Write(textFrame([]byte(`42["on_asset_update",{}]`)))
		_ = rw.Flush()
		b := make([]byte, 5)
		_, _ = io.ReadFull(rw, b)
		fromClient <- b
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	dial := func(on bool) (*bufio.Reader, net.Conn) {
		s := NewServer(Config{UpstreamURL: u, AppSync: on}, &fakeConverter{}, customlogger.NewWithLevel(nil, "error", ""))
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		t.Cleanup(s.Cancel)
		conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		_, _ = io.WriteString(conn, "GET "+socketPath+"?EIO=4&transport=websocket HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n"+
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: a2V5\r\nSec-WebSocket-Extensions: permessage-deflate\r\nAuthorization: Bearer token\r\n\r\n")
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("handshake: %v %v", resp, err)
		}
		return br, conn
	}

	br, conn := dial(true)
	if gotExtensions != "" {
		t.Errorf("compression was offered to Immich (%q): the messages would not be readable", gotExtensions)
	}
	var texts []string
	for i := 0; i < 6; i++ {
		first, payload := readFrame(t, br)
		if first != 0x81 {
			t.Fatalf("frame %d starts with %#x", i, first)
		}
		texts = append(texts, string(payload))
	}
	if texts[0] != `0{"sid":"abc"}` || texts[5] != `42["on_asset_update",{}]` {
		t.Fatalf("messages = %q", texts)
	}
	for i, id := range []string{plain, kept, realAsset} {
		if texts[1+i] != string(uploadReady("AssetUploadReadyV2", id)) {
			t.Errorf("the message of %s was changed: %s", id, texts[1+i])
		}
	}
	// The added message: the same event, for the hidden entry.
	var msg []json.RawMessage
	if !strings.HasPrefix(texts[4], "42[") || json.Unmarshal([]byte(texts[4][2:]), &msg) != nil || len(msg) != 2 || string(msg[0]) != `"AssetUploadReadyV2"` {
		t.Fatalf("added message = %s", texts[4])
	}
	var data struct {
		Asset map[string]any `json:"asset"`
		Exif  map[string]any `json:"exif"`
	}
	_ = json.Unmarshal(msg[1], &data)
	a := data.Asset
	if a["id"] != hiddenID(realAsset) || a["checksum"] != abcBase64 || a["visibility"] != "hidden" || a["libraryId"] != hiddenLibraryID ||
		a["ownerId"] != owner || a["thumbhash"] != nil || a["isFavorite"] != false || a["originalFileName"] != hiddenFileName {
		t.Errorf("hidden asset = %v", a)
	}
	if a["somethingNew"] != float64(7) {
		t.Errorf("a field this version does not know was dropped: %v", a)
	}
	if data.Exif["assetId"] != hiddenID(realAsset) || data.Exif["make"] != "Camera" {
		t.Errorf("hidden exif = %v", data.Exif)
	}

	// The other direction is untouched.
	_, _ = conn.Write([]byte("hello"))
	select {
	case b := <-fromClient:
		if string(b) != "hello" {
			t.Errorf("Immich got %q from the client", b)
		}
	case <-time.After(10 * time.Second):
		t.Error("what the client sent did not reach Immich")
	}

	// Turned off, the websocket is passed on as it is.
	br, _ = dial(false)
	if gotExtensions != "permessage-deflate" {
		t.Errorf("the handshake was changed although the feature is off: %q", gotExtensions)
	}
	for i := 0; i < 5; i++ {
		if _, payload := readFrame(t, br); strings.Contains(string(payload), hiddenFileName) {
			t.Errorf("a message was added although the feature is off: %s", payload)
		}
	}
}
