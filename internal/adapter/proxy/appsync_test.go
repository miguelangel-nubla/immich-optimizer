package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

const (
	realAsset = "2f908a71-185e-4fb6-960c-278a8d87e66b"
	owner     = "13798d0a-3726-435b-a2a5-477799f27613"
	// SHA-1 of "abc", hex and base64.
	abcHex    = "a9993e364706816aba3e25717850c26c9cd0d89d"
	abcBase64 = "qZk+NkcGgWq6PiVxeFDCbJzQ2J0="
)

func TestHiddenIDIsReversibleAndNeverAnOrdinaryUUID(t *testing.T) {
	for _, id := range []string{realAsset, "00000000-0000-4000-8000-000000000000", "ffffffff-ffff-7fff-bfff-ffffffffffff", "0195A2B4-0000-7000-9000-AAAAAAAAAAAA"} {
		h := hiddenID(id)
		if h == "" || h == id || len(h) != 36 {
			t.Fatalf("hiddenID(%s) = %q", id, h)
		}
		back, ok := realID(h)
		if !ok || !strings.EqualFold(back, id) {
			t.Errorf("realID(%s) = %q, want %s", h, back, id)
		}
		if _, ok := realID(id); ok {
			t.Errorf("an ordinary id %s was taken for a hidden one", id)
		}
		if hiddenID(h) != "" {
			t.Errorf("a hidden id %s got a hidden id of its own", h)
		}
	}
	for _, bad := range []string{"", "not-a-uuid", realAsset + "x", "2f908a71-185e-4fb6-960c-278a8d87e66"} {
		if hiddenID(bad) != "" {
			t.Errorf("hiddenID(%q) must be empty", bad)
		}
	}
	if p, changed := rewriteHiddenIDs("/api/assets/" + hiddenID(realAsset) + "/thumbnail"); !changed || p != "/api/assets/"+realAsset+"/thumbnail" {
		t.Errorf("path = %q", p)
	}
	if _, changed := rewriteHiddenIDs("/api/assets/" + realAsset + "/thumbnail"); changed {
		t.Error("an ordinary path was rewritten")
	}
}

func line(typ string, data any, ack string) string {
	d, _ := json.Marshal(data)
	b, _ := json.Marshal(syncLine{Type: typ, Data: d, Ack: ack})
	return string(b) + "\n"
}

// marker is the sync line of a marker. value says what it holds in a short
// form (sha1, kept, state, task), which is laid out as the record really is.
func marker(assetID string, short map[string]any) string {
	value := map[string]any{"v": 1}
	if s, ok := short["sha1"]; ok {
		value["original"] = map[string]any{"sha1": s, "size": 3}
	}
	if task, ok := short["task"]; ok {
		value["conversion"] = map[string]any{"task": map[string]any{"name": task}}
	}
	if state, ok := short["state"]; ok {
		value["library"] = map[string]any{"state": state}
	}
	if short["kept"] == true {
		value["kept"] = map[string]any{"note": "no gain"}
	}
	return line("AssetMetadataV1", map[string]any{"assetId": assetID, "key": "immich-optimizer", "value": value}, "AssetMetadataV1|u1")
}

// run feeds a stream through the injector and returns the lines that come out.
func run(t *testing.T, assetType, stream string) ([]syncLine, *syncInjector) {
	t.Helper()
	return runWith(t, &syncInjector{assetType: assetType, ownerID: func() (string, error) { return owner, nil }}, stream)
}

func runWith(t *testing.T, in *syncInjector, stream string) ([]syncLine, *syncInjector) {
	t.Helper()
	var out bytes.Buffer
	if err := in.copyStream(&out, strings.NewReader(stream), func() {}); err != nil {
		t.Fatal(err)
	}
	var lines []syncLine
	for _, raw := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if raw == "" {
			continue
		}
		var l syncLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("output line is not JSON: %q", raw)
		}
		lines = append(lines, l)
	}
	return lines, in
}

func TestMarkerLineGetsAHiddenEntry(t *testing.T) {
	asset := line("AssetV2", map[string]any{"id": realAsset, "checksum": "Yk1+", "ownerId": owner}, "AssetV2|u0")
	other := line("AssetMetadataV1", map[string]any{"assetId": realAsset, "key": "mobile-app", "value": map[string]any{"iCloudId": "X"}}, "AssetMetadataV1|u2")
	stream := asset + marker(realAsset, map[string]any{"v": 1, "sha1": abcHex, "task": "jpeg-xl"}) + other

	lines, in := run(t, "AssetV2", stream)
	if len(lines) != 4 || in.Injected != 1 {
		t.Fatalf("%d lines, %d injected: %+v", len(lines), in.Injected, lines)
	}
	// The original lines pass untouched, in order; the hidden entry follows its marker.
	if lines[0].Ack != "AssetV2|u0" || lines[1].Type != "AssetMetadataV1" || lines[3].Ack != "AssetMetadataV1|u2" {
		t.Errorf("stream order changed: %+v", lines)
	}
	h := lines[2]
	var d map[string]any
	if err := json.Unmarshal(h.Data, &d); err != nil {
		t.Fatal(err)
	}
	if h.Type != "AssetV2" || !strings.HasPrefix(h.Ack, hiddenAckPrefix) {
		t.Errorf("hidden line = %+v", h)
	}
	if d["id"] != hiddenID(realAsset) || d["checksum"] != abcBase64 || d["ownerId"] != owner ||
		d["visibility"] != "hidden" || d["libraryId"] == nil || d["deletedAt"] != nil {
		t.Errorf("hidden entry = %v", d)
	}
	// Every field of an asset line is present, so the app can parse it.
	for _, k := range []string{"id", "ownerId", "originalFileName", "thumbhash", "checksum", "fileCreatedAt", "fileModifiedAt",
		"createdAt", "localDateTime", "duration", "type", "deletedAt", "isFavorite", "visibility", "livePhotoVideoId",
		"stackId", "libraryId", "width", "height", "isEdited"} {
		if _, ok := d[k]; !ok {
			t.Errorf("hidden entry has no %q", k)
		}
	}
	// An older client gets the older line type.
	if lines, _ := run(t, "AssetV1", stream); lines[2].Type != "AssetV1" {
		t.Errorf("type = %s", lines[2].Type)
	}
}

func TestNoHiddenEntryWhenItWouldBeWrong(t *testing.T) {
	cases := map[string]string{
		"kept original":          marker(realAsset, map[string]any{"v": 1, "sha1": abcHex, "kept": true}),
		"unfinished replacement": marker(realAsset, map[string]any{"v": 1, "sha1": abcHex, "state": "pending"}),
		"no checksum":            marker(realAsset, map[string]any{"v": 1}),
		"bad checksum":           marker(realAsset, map[string]any{"v": 1, "sha1": "zz"}),
		"another key":            line("AssetMetadataV1", map[string]any{"assetId": realAsset, "key": "mobile-app", "value": map[string]any{"sha1": abcHex}}, "a"),
		"not an asset id":        marker("nope", map[string]any{"v": 1, "sha1": abcHex}),
	}
	for name, stream := range cases {
		if lines, in := run(t, "AssetV2", stream); len(lines) != 1 || in.Injected != 0 {
			t.Errorf("%s: %d lines out", name, len(lines))
		}
	}
	// The client did not ask for assets: nothing may be added.
	if lines, _ := run(t, "", marker(realAsset, map[string]any{"v": 1, "sha1": abcHex})); len(lines) != 1 {
		t.Errorf("added a line although no assets were requested")
	}
	// The user is unknown: pass the stream through.
	in := &syncInjector{assetType: "AssetV2", ownerID: func() (string, error) { return "", io.ErrUnexpectedEOF }}
	var out bytes.Buffer
	_ = in.copyStream(&out, strings.NewReader(marker(realAsset, map[string]any{"v": 1, "sha1": abcHex})), func() {})
	if strings.Count(out.String(), "\n") != 1 || in.ownerErr == nil {
		t.Errorf("unknown user: %q", out.String())
	}
}

func TestDeletionsReachTheHiddenEntry(t *testing.T) {
	stream := line("AssetDeleteV1", map[string]any{"assetId": realAsset}, "AssetDeleteV1|d1") +
		line("AssetMetadataDeleteV1", map[string]any{"assetId": realAsset, "key": "immich-optimizer"}, "AssetMetadataDeleteV1|d2") +
		line("AssetMetadataDeleteV1", map[string]any{"assetId": realAsset, "key": "mobile-app"}, "AssetMetadataDeleteV1|d3")
	lines, _ := run(t, "AssetV2", stream)
	if len(lines) != 5 {
		t.Fatalf("%d lines: %+v", len(lines), lines)
	}
	for _, i := range []int{1, 3} {
		var d map[string]string
		_ = json.Unmarshal(lines[i].Data, &d)
		if lines[i].Type != "AssetDeleteV1" || d["assetId"] != hiddenID(realAsset) {
			t.Errorf("line %d = %+v", i, lines[i])
		}
	}
	// The added delete continues the run of real deletes, so it carries the
	// real token: the app acknowledges only the last line of a run.
	if lines[1].Ack != "AssetDeleteV1|d1" {
		t.Errorf("delete ack = %q", lines[1].Ack)
	}
	if !strings.HasPrefix(lines[3].Ack, hiddenAckPrefix) {
		t.Errorf("metadata-delete ack = %q", lines[3].Ack)
	}
}

// End to end through the proxy: the stream is edited, the proxy's own tokens
// never reach Immich, and requests about a hidden entry are answered with the
// real asset.
// hiddenLines returns what the hidden entries in lines say about being in the
// trash, in order: "" for not in the trash.
func hiddenLines(t *testing.T, lines []syncLine) []string {
	t.Helper()
	var out []string
	for _, l := range lines {
		var d struct {
			ID        string  `json:"id"`
			Checksum  string  `json:"checksum"`
			DeletedAt *string `json:"deletedAt"`
		}
		if l.Type != "AssetV2" || json.Unmarshal(l.Data, &d) != nil || d.ID != hiddenID(realAsset) {
			continue
		}
		if d.Checksum != abcBase64 {
			t.Errorf("hidden entry with checksum %q", d.Checksum)
		}
		if d.DeletedAt == nil {
			out = append(out, "")
		} else {
			out = append(out, *d.DeletedAt)
		}
	}
	return out
}

func TestHiddenEntryFollowsTheTrash(t *testing.T) {
	const when = "2026-01-02T03:04:05.000Z"
	asked := 0
	withMarker := func() *syncInjector {
		return &syncInjector{assetType: "AssetV2", ownerID: func() (string, error) { return owner, nil },
			marker: func(id string) *entity.Marker {
				asked++
				if id == realAsset {
					return &entity.Marker{V: 1, SHA1: abcHex}
				}
				return nil
			}}
	}
	trashed := line("AssetV2", map[string]any{"id": realAsset, "deletedAt": when}, "AssetV2|u5")
	restored := line("AssetV2", map[string]any{"id": realAsset, "deletedAt": nil}, "AssetV2|u6")
	plain := line("AssetV2", map[string]any{"id": "5f908a71-185e-4fb6-960c-278a8d87e66b", "deletedAt": nil}, "AssetV2|u7")
	mark := marker(realAsset, map[string]any{"v": 1, "sha1": abcHex})

	// Trashing sends only the asset's line: the marker is asked for.
	lines, _ := runWith(t, withMarker(), trashed)
	if got := hiddenLines(t, lines); len(got) != 1 || got[0] != when || len(lines) != 2 || lines[1].Ack != "AssetV2|u5" {
		t.Fatalf("after trashing: %v (%d lines)", got, len(lines))
	}
	// Restoring too.
	lines, _ = runWith(t, withMarker(), restored)
	if got := hiddenLines(t, lines); len(got) != 1 || got[0] != "" {
		t.Fatalf("after restoring: %v", got)
	}
	// A first sync of an asset that is in the trash: one hidden entry, in the trash.
	lines, _ = runWith(t, withMarker(), trashed+mark)
	if got := hiddenLines(t, lines); len(got) != 1 || got[0] != when {
		t.Fatalf("first sync of a trashed asset: %v", got)
	}
	// An asset without a marker gets nothing.
	if lines, _ = runWith(t, withMarker(), plain); len(lines) != 1 {
		t.Fatalf("an asset without a marker got %d lines", len(lines))
	}

	// A long stream does not ask for every asset: beyond the limit only the
	// trashed ones are asked for, and marker lines still do their work.
	asked = 0
	var long strings.Builder
	for i := 0; i < maxLookups+50; i++ {
		long.WriteString(plain)
	}
	long.WriteString(restored + mark)
	lines, _ = runWith(t, withMarker(), long.String())
	if asked != maxLookups {
		t.Errorf("asked Immich for %d markers, want %d", asked, maxLookups)
	}
	if got := hiddenLines(t, lines); len(got) != 1 || got[0] != "" {
		t.Errorf("long stream: %v", got)
	}
}

func TestAppSyncThroughTheProxy(t *testing.T) {
	var acks [][]string
	var paths []string
	meCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/users/me":
			meCalls++
			if r.Header.Get("Authorization") != "Bearer token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"id":"`+owner+`"}`)
		case syncStreamPath:
			w.Header().Set("Content-Type", "application/jsonlines+json")
			_, _ = io.WriteString(w, marker(realAsset, map[string]any{"v": 1, "sha1": abcHex}))
		case syncAckPath:
			var body struct {
				Acks []string `json:"acks"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			acks = append(acks, body.Acks)
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, "thumb")
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)

	newProxy := func() *httptest.Server {
		s := NewServer(Config{UpstreamURL: u}, &fakeConverter{}, customlogger.NewWithLevel(nil, "error", ""))
		return httptest.NewServer(s)
	}
	do := func(base, method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer token")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	on := newProxy()
	defer on.Close()
	status, out := do(on.URL, http.MethodPost, syncStreamPath, `{"types":["AssetsV2","AssetMetadataV1"]}`)
	if status != 200 || strings.Count(out, "\n") != 2 || !strings.Contains(out, abcBase64) || !strings.Contains(out, hiddenID(realAsset)) {
		t.Fatalf("sync through the proxy: %d %q", status, out)
	}
	// A second sync does not ask Immich for the user again.
	do(on.URL, http.MethodPost, syncStreamPath, `{"types":["AssetsV2","AssetMetadataV1"]}`)
	if meCalls != 1 {
		t.Errorf("asked for the user %d times, want 1", meCalls)
	}

	// Acknowledgements: proxy-made tokens are removed; one that is alone is answered by the proxy.
	if s, _ := do(on.URL, http.MethodPost, syncAckPath, `{"acks":["AssetMetadataV1|u1","`+hiddenAckPrefix+realAsset+`"]}`); s != http.StatusNoContent {
		t.Errorf("ack status %d", s)
	}
	if s, _ := do(on.URL, http.MethodPost, syncAckPath, `{"acks":["`+hiddenAckPrefix+realAsset+`"]}`); s != http.StatusNoContent {
		t.Errorf("ack status %d", s)
	}
	if len(acks) != 1 || len(acks[0]) != 1 || acks[0][0] != "AssetMetadataV1|u1" {
		t.Errorf("acks that reached Immich = %v", acks)
	}

	// A request about the hidden entry reaches Immich as one about the real asset.
	paths = nil
	if s, body := do(on.URL, http.MethodGet, "/api/assets/"+hiddenID(realAsset)+"/thumbnail", ""); s != 200 || body != "thumb" {
		t.Errorf("thumbnail: %d %q", s, body)
	}
	if len(paths) != 1 || paths[0] != "GET /api/assets/"+realAsset+"/thumbnail" {
		t.Errorf("Immich saw %v", paths)
	}
}

// The app can name a hidden entry in the body of a request: the iOS app when
// it records iCloud ids, and both apps when they stack an edited photo on
// its earlier upload.
func TestHiddenIDsInRequestBodies(t *testing.T) {
	const second = "7f908a71-185e-4fb6-960c-278a8d87e66b"
	type seen struct{ method, path, body string }
	var got []seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if n := r.Header.Get("Content-Length"); n != "" && n != strconv.Itoa(len(b)) {
			t.Errorf("%s %s: Content-Length %s for %d bytes", r.Method, r.URL.Path, n, len(b))
		}
		got = append(got, seen{r.Method, r.URL.Path, string(b)})
		_, _ = io.WriteString(w, `["from immich"]`)
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	proxy := httptest.NewServer(NewServer(Config{UpstreamURL: u}, &fakeConverter{}, customlogger.NewWithLevel(nil, "error", "")))
	defer proxy.Close()
	send := func(method, path, ctype, body string) string {
		t.Helper()
		req, _ := http.NewRequest(method, proxy.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", ctype)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: status %d", method, path, resp.StatusCode)
		}
		return string(b)
	}
	item := func(id string) string {
		return `{"assetId":"` + id + `","key":"mobile-app","value":{"iCloudId":"X"}}`
	}

	// A batch of iCloud ids: the hidden entries are left out, the rest goes on.
	send(http.MethodPut, bulkMetadataPath, "application/json", `{"items":[`+item(realAsset)+`,`+item(hiddenID(realAsset))+`,`+item(second)+`]}`)
	if len(got) != 1 || strings.Contains(got[0].body, hiddenID(realAsset)) || strings.Count(got[0].body, `"assetId"`) != 2 ||
		!strings.Contains(got[0].body, realAsset) || !strings.Contains(got[0].body, second) || !strings.Contains(got[0].body, `"iCloudId":"X"`) {
		t.Fatalf("the metadata batch reached Immich as %+v", got)
	}
	// A batch of hidden entries only does not reach Immich at all.
	got = nil
	if out := send(http.MethodPut, bulkMetadataPath, "application/json", `{"items":[`+item(hiddenID(realAsset))+`]}`); len(got) != 0 || out != "[]" {
		t.Fatalf("a batch with nothing left: answer %q, Immich saw %+v", out, got)
	}

	// Anywhere else a hidden id means the real asset: an edited photo is
	// stacked on the converted asset of its earlier version.
	got = nil
	send(http.MethodPost, "/api/stacks", "application/json; charset=utf-8", `{"assetIds":["`+second+`","`+hiddenID(realAsset)+`"]}`)
	if len(got) != 1 || got[0].body != `{"assetIds":["`+second+`","`+realAsset+`"]}` {
		t.Fatalf("the stack request reached Immich as %+v", got)
	}

	// Bodies without a hidden id, and bodies that are not JSON, are not touched.
	got = nil
	plain := `{"assetIds":["` + second + `","` + realAsset + `"],"note":"  spacing   kept "}`
	send(http.MethodPost, "/api/stacks", "application/json", plain)
	text := `"` + hiddenID(realAsset) + `"`
	send(http.MethodPost, "/api/other", "text/plain", text)
	if len(got) != 2 || got[0].body != plain || got[1].body != text {
		t.Fatalf("untouched bodies reached Immich as %+v", got)
	}
}
