package proxy

import (
	"bufio"
	"bytes"
	"mime/multipart"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// The sync stream comes from Immich, and the proxy adds lines to it. Whatever
// it holds, every line Immich sent reaches the app unchanged and in order;
// the proxy only adds.
func FuzzSyncStream(f *testing.F) {
	f.Add(marker(realAsset, map[string]any{"sha1": abcHex}) +
		line("AssetV2", map[string]any{"id": realAsset, "deletedAt": "2026-01-02T03:04:05.000Z"}, "AssetV2|u5") +
		line("AssetDeleteV1", map[string]any{"assetId": realAsset}, "AssetDeleteV1|u6") +
		line("AssetMetadataV1", map[string]any{"assetId": realAsset, "key": "mobile-app", "value": map[string]any{"iCloudId": "x"}}, "m|1"))
	f.Add("not json\n{\"type\":\"AssetV2\"}\n")
	f.Fuzz(func(t *testing.T, stream string) {
		in := &syncInjector{assetType: "AssetV2", ownerID: func() (string, error) { return owner, nil },
			marker: func(string) *entity.Marker { return &entity.Marker{V: 1, SHA1: abcHex} },
			state:  func(string) (string, bool) { return "", true }}
		var out bytes.Buffer
		if err := in.copyStream(&out, strings.NewReader(stream), func() {}); err != nil {
			t.Fatal(err)
		}
		// Each input line, newline-terminated, is found in order.
		rest := out.String()
		lines := strings.SplitAfter(stream, "\n")
		for _, l := range lines {
			if l == "" {
				continue
			}
			if !strings.HasSuffix(l, "\n") {
				l += "\n" // the last line gets one
			}
			i := strings.Index(rest, l)
			if i < 0 {
				t.Fatalf("line %q lost or changed", l)
			}
			rest = rest[i+len(l):]
		}
	})
}

// Websocket frames from either side pass through the proxy. Without anything
// to add, what comes out is exactly what went in, up to where the input
// stops being a frame.
func FuzzSocketPump(f *testing.F) {
	f.Add(textFrame([]byte(`42["AssetUploadReadyV1",{"asset":{"id":"x"}}]`)))
	f.Add(append(textFrame([]byte("2")), 0x88, 0x00))
	f.Add([]byte{0x81, 0xfe, 0x00, 0x05, 1, 2, 3, 4, 'a', 'b', 'c', 'd', 'e'})
	f.Add([]byte{0x82, 127, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		var out bytes.Buffer
		p := &socketPump{hidden: func([]byte) []byte { return nil }}
		_ = p.run(&out, bufio.NewReader(bytes.NewReader(data)))
		if !bytes.HasPrefix(data, out.Bytes()) {
			t.Fatalf("the pump changed what it passed on:\nin  %x\nout %x", data, out.Bytes())
		}
	})
}

// An upload's body is whatever the client sends. Reading it must never
// crash, and must not leave temporary files behind once its parts are
// cleaned up.
func FuzzExtractTargets(f *testing.F) {
	const boundary = "b0undary"
	build := func(parts ...[3]string) string {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		_ = mw.SetBoundary(boundary)
		for _, p := range parts {
			if p[1] != "" {
				w, _ := mw.CreateFormFile(p[0], p[1])
				_, _ = w.Write([]byte(p[2]))
			} else {
				_ = mw.WriteField(p[0], p[2])
			}
		}
		_ = mw.Close()
		return b.String()
	}
	f.Add(build([3]string{"deviceAssetId", "", "a"}, [3]string{"assetData", "IMG_1.jpg", "jpeg"}, [3]string{"filename", "", "IMG_1.jpg"}))
	f.Add(build([3]string{"assetData", "../../etc/passwd", "x"}, [3]string{"assetData", "b.png", "y"}))
	f.Add("--" + boundary + "\r\nContent-Disposition: form-data; name=\"assetData\"; filename=\"a.jpg\"\r\n\r\ntrunc")
	u, _ := url.Parse("http://immich")
	f.Fuzz(func(t *testing.T, body string) {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		spool := filepath.Join(t.TempDir(), "spool")
		if err := os.WriteFile(spool, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		s := &Server{cfg: Config{UpstreamURL: u}}
		targets, _, _, _ := s.extractTargets(spool, boundary)
		for _, tp := range targets {
			if filepath.Dir(tp.srcPath) != filepath.Clean(tmp) {
				t.Fatalf("a part was written outside the temporary folder: %s", tp.srcPath)
			}
			if strings.ContainsAny(filepath.Base(tp.srcPath), `/\`) {
				t.Fatalf("odd temporary name %q", tp.srcPath)
			}
			_ = os.Remove(tp.srcPath)
		}
		if left, _ := os.ReadDir(tmp); len(left) != 0 {
			t.Fatalf("%d temporary file(s) left", len(left))
		}
	})
}

func TestUpstreamTargetKeepsEscapes(t *testing.T) {
	for _, c := range []struct{ base, req, want string }{
		{"http://immich:2283", "/api/x/a%2Fb?q=1", "http://immich:2283/api/x/a%2Fb?q=1"},
		{"http://host/immich/", "/api/assets", "http://host/immich/api/assets"},
		{"http://immich", "/api/a%20b", "http://immich/api/a%20b"},
	} {
		base, _ := url.Parse(c.base)
		req, _ := url.Parse(c.req)
		s := &Server{cfg: Config{UpstreamURL: base}}
		if got := s.upstreamTarget(req); got != c.want {
			t.Errorf("%s + %s = %s, want %s", c.base, c.req, got, c.want)
		}
	}
}
