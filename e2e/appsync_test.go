//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type streamLine struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
	Ack  string         `json:"ack"`
}

// syncThrough runs the mobile app's sync request against base and returns
// the lines of the answer.
func syncThrough(t *testing.T, u *user, base string) []streamLine {
	t.Helper()
	body := `{"types":["AuthUsersV1","UsersV1","AssetsV2","AssetExifsV1","AssetMetadataV1"]}`
	req, _ := http.NewRequest(http.MethodPost, base+"/api/sync/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(u.session[0], u.session[1])
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync through %s: status %d", base, resp.StatusCode)
	}
	var lines []streamLine
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var l streamLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("sync line is not JSON: %q", sc.Text())
		}
		lines = append(lines, l)
	}
	return lines
}

// ackAll acknowledges every line the way the app does: the last token of
// each run of same-type lines.
func ackAll(t *testing.T, u *user, base string, lines []streamLine) {
	t.Helper()
	var acks []string
	for i, l := range lines {
		if i == len(lines)-1 || lines[i+1].Type != l.Type {
			acks = append(acks, l.Ack)
		}
	}
	for _, a := range acks {
		b, _ := json.Marshal(map[string][]string{"acks": {a}})
		req, _ := http.NewRequest(http.MethodPost, base+"/api/sync/ack", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(u.session[0], u.session[1])
		resp, err := httpc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("ack %q through %s: status %d", a, base, resp.StatusCode)
		}
	}
}

func hiddenLines(lines []streamLine) []streamLine {
	var out []streamLine
	for _, l := range lines {
		if l.Type == "AssetV2" && l.Data["visibility"] == "hidden" && l.Data["libraryId"] != nil {
			out = append(out, l)
		}
	}
	return out
}

// The experimental app sync, against Immich's real sync stream: what a phone
// would receive, acknowledge and be told to delete.
func TestProxyAppSyncAddsHiddenEntries(t *testing.T) {
	u := newUser(t)
	undone := sharedDir(t)
	_, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy", "IO_PROXY_EXPERIMENTAL_APP_SYNC": "true"}, true, undone+":/undone")

	// One converted upload and one that is passed through unchanged.
	src := filepath.Join(mediaDir, "photo.jpg")
	status, out := u.upload(t, proxy, src, "photo.jpg", nil)
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, out["_body"])
	}
	converted := u.waitReady(t, out["id"])
	status, out = u.upload(t, proxy, filepath.Join(mediaDir, "photo.heic"), "photo.heic", nil)
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, out["_body"])
	}
	u.waitReady(t, out["id"])

	data, _ := os.ReadFile(src)
	sum := sha1.Sum(data)
	want := base64.StdEncoding.EncodeToString(sum[:])

	// Straight from Immich the phone only learns the converted checksum.
	for _, l := range syncThrough(t, u, immichURL) {
		if l.Data["checksum"] == want {
			t.Fatal("Immich itself already reports the original's checksum")
		}
	}

	// Through the proxy there is one hidden entry, with the original's checksum.
	lines := syncThrough(t, u, proxy)
	hidden := hiddenLines(lines)
	if len(hidden) != 1 {
		t.Fatalf("%d hidden entries, want 1 (only the converted asset has a marker)", len(hidden))
	}
	h := hidden[0].Data
	if h["checksum"] != want || h["ownerId"] != u.id || h["deletedAt"] != nil || h["id"] == converted.ID {
		t.Errorf("hidden entry = %v, want checksum %s and owner %s", h, want, u.id)
	}
	// The real asset is still there, unchanged.
	real := false
	for _, l := range lines {
		if l.Type == "AssetV2" && l.Data["id"] == converted.ID {
			real = l.Data["checksum"] == converted.Checksum && l.Data["visibility"] == "timeline"
		}
	}
	if !real {
		t.Error("the real asset's line is missing or was changed")
	}

	// A request about the hidden entry is answered with the real asset.
	hid, _ := h["id"].(string)
	req, _ := http.NewRequest(http.MethodGet, proxy+"/api/assets/"+hid+"/thumbnail", nil)
	req.Header.Set(u.session[0], u.session[1])
	resp, err := httpc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("thumbnail of the hidden entry: %v %v", resp, err)
	}
	if resp != nil {
		resp.Body.Close()
	}

	// The phone acknowledges everything, including the proxy's own line.
	// Immich accepts it, and the next sync has nothing left to send.
	ackAll(t, u, proxy, lines)
	if again := syncThrough(t, u, proxy); len(hiddenLines(again)) != 0 {
		t.Errorf("the hidden entry was sent again after being acknowledged: %d line(s)", len(again))
	}

	// Deleting the converted asset tells the phone to drop the hidden entry too.
	must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{converted.ID}, "force": true}, nil)
	deadline := time.Now().Add(time.Minute)
	for {
		var deleted []string
		for _, l := range syncThrough(t, u, proxy) {
			if l.Type == "AssetDeleteV1" {
				id, _ := l.Data["assetId"].(string)
				deleted = append(deleted, id)
			}
		}
		if contains(deleted, converted.ID) {
			if !contains(deleted, hid) {
				t.Errorf("the hidden entry was not deleted with its asset: %v", deleted)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Immich never reported the deletion")
		}
		time.Sleep(time.Second)
	}
}
