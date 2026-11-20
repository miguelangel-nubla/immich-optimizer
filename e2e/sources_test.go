//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// undoneFolders returns the hash folders in an undone directory.
func undoneFolders(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) == 40 {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestProxyConvertsUploads(t *testing.T) {
	u := newUser(t)
	undone := sharedDir(t)
	_, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")

	// A JPEG is stored as JPEG XL with a marker pointing at the original.
	src := filepath.Join(mediaDir, "photo.jpg")
	status, out := u.upload(t, proxy, src, "photo.jpg", map[string]string{"metadata": `[{"key":"mobile-app","value":{"iCloudId":"X:1"}}]`})
	if status != http.StatusCreated {
		t.Fatalf("upload through the proxy: %d %s", status, out["_body"])
	}
	a := u.waitReady(t, out["id"])
	if a.OriginalFileName != "photo.jxl" || a.OriginalMimeType != "image/jxl" {
		t.Errorf("stored asset: %+v", a)
	}
	m := u.marker(t, a.ID)
	if m == nil || m["sha1"] != fileSHA1(t, src) || m["source"] != "proxy" || m["task"] != "jpeg-xl" {
		t.Errorf("marker = %v", m)
	}
	// The client's own metadata item survived next to it.
	var items []struct {
		Key string `json:"key"`
	}
	must(t, u.key, http.MethodGet, "/assets/"+a.ID+"/metadata", nil, &items)
	if len(items) != 2 {
		t.Errorf("metadata items = %+v", items)
	}

	// The same file again: Immich recognises it, nothing is stored twice.
	status, out = u.upload(t, proxy, src, "photo.jpg", nil)
	if status != http.StatusOK || out["status"] != "duplicate" || out["id"] != a.ID {
		t.Errorf("second upload: %d %s", status, out["_body"])
	}

	// A motion photo: the photo is kept, the video dropped.
	status, out = u.upload(t, proxy, filepath.Join(mediaDir, "motion.jpg"), "motion.jpg", nil)
	if status != http.StatusCreated {
		t.Fatalf("motion photo upload: %d %s", status, out["_body"])
	}
	if mm := u.marker(t, out["id"]); mm == nil || mm["motion_photos"] != "drop_video" {
		t.Errorf("motion photo marker = %v", mm)
	}

	// A file the tasks cannot convert: the client gets an error, nothing is
	// uploaded, and the original is kept in the undone folder.
	broken := filepath.Join(t.TempDir(), "broken.jpg")
	if err := os.WriteFile(broken, []byte("this is not a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(u.live(t))
	status, _ = u.upload(t, proxy, broken, "broken.jpg", nil)
	if status != http.StatusBadGateway {
		t.Errorf("broken upload answered %d, want 502", status)
	}
	if len(u.live(t)) != before {
		t.Error("a broken file reached Immich")
	}
	folders := undoneFolders(t, undone)
	if len(folders) != 1 || folders[0] != fileSHA1(t, broken) {
		t.Fatalf("undone folders = %v, want one named after the file's SHA-1", folders)
	}
	if fileSHA1(t, filepath.Join(undone, folders[0], "original.file")) != fileSHA1(t, broken) {
		t.Error("undone copy differs from the upload")
	}
	if _, err := os.Stat(filepath.Join(undone, folders[0], "metadata.json")); err != nil {
		t.Errorf("undone metadata: %v", err)
	}

	// The audit log has one line per asset operation, and nothing else.
	audit := readLog(t, filepath.Join(undone, "audit.log"))
	var got []string
	for _, e := range audit {
		if e["source"] != "proxy" && e["event"] != "started" {
			t.Errorf("unexpected source in %v", e)
		}
		got = append(got, fmt.Sprint(e["event"]))
	}
	if strings.Join(got, " ") != "started uploaded duplicate uploaded failed" {
		t.Errorf("audit events = %v", got)
	}
	if audit[1]["asset"] != a.ID || audit[1]["outcome"] != "optimized" || audit[1]["original_sha1"] != fileSHA1(t, src) {
		t.Errorf("first upload audit entry = %v", audit[1])
	}
	if last := audit[len(audit)-1]; last["file"] != "broken.jpg" || !strings.HasSuffix(fmt.Sprint(last["undone"]), folders[0]) {
		t.Errorf("failure audit entry = %v", last)
	}

	// Everything else is passed straight through to Immich.
	var me struct {
		ID string `json:"id"`
	}
	req, _ := http.NewRequest(http.MethodGet, proxy+"/api/users/me", nil)
	req.Header.Set(u.key[0], u.key[1])
	resp, err := httpc.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("pass-through request: %v", err)
	}
	resp.Body.Close()
	_ = me
}

func TestWatcherUploadsAndCleansUp(t *testing.T) {
	u := newUser(t)
	watch, undone := sharedDir(t), sharedDir(t)
	startOptimizer(t, map[string]string{"IO_MODE": "watcher", "IO_IMMICH_API_KEY": u.keyS}, false, watch+":/watch", undone+":/undone")

	// A photo with no date inside, dated 2016 on disk, in a subfolder.
	data, err := os.ReadFile(filepath.Join(mediaDir, "plain.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(watch, "trip"), 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Join(watch, "trip"), 0o777)
	when := time.Date(2016, 7, 8, 9, 10, 11, 0, time.UTC)
	// Written under a hidden name first, as sync tools do, then renamed.
	tmp := filepath.Join(watch, "trip", ".scan.jpg.partial")
	if err := os.WriteFile(tmp, data, 0o666); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(tmp, when, when)
	final := filepath.Join(watch, "trip", "scan.jpg")
	if err := os.Rename(tmp, final); err != nil {
		t.Fatal(err)
	}
	// A hidden file that must be left alone.
	hidden := filepath.Join(watch, ".DS_Store")
	if err := os.WriteFile(hidden, []byte("junk"), 0o666); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	var got []asset
	for {
		got = u.live(t)
		if len(got) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if len(got) != 1 {
		t.Fatalf("%d assets in Immich, want 1", len(got))
	}
	a := u.waitReady(t, got[0].ID)
	if a.OriginalFileName != "scan.jxl" {
		t.Errorf("file name = %s", a.OriginalFileName)
	}
	if a.FileCreatedAt != "2016-07-08T09:10:11.000Z" {
		t.Errorf("asset date = %s, want the original file's date", a.FileCreatedAt)
	}
	if m := u.marker(t, a.ID); m == nil || m["source"] != "watcher" {
		t.Errorf("marker = %v", m)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Error("the source file was not removed after the upload")
	}
	if _, err := os.Stat(hidden); err != nil {
		t.Error("the hidden file was touched")
	}
	audit := readLog(t, filepath.Join(undone, "audit.log"))
	if len(audit) != 2 || audit[0]["event"] != "started" || audit[1]["event"] != "uploaded" || audit[1]["source"] != "watcher" || audit[1]["asset"] != a.ID || audit[1]["file"] != "scan.jpg" {
		t.Errorf("audit log = %v", audit)
	}
	if f := undoneFolders(t, undone); len(f) != 0 {
		t.Errorf("unexpected undone folders: %v", f)
	}
}

// The audit log is mandatory: without a writable one the optimizer does not
// start.
func TestRefusesToStartWithoutAuditLog(t *testing.T) {
	u := newUser(t)
	undone := sharedDir(t)
	args := append([]string{"run", "--rm"}, dockerArgs(map[string]string{
		"IO_MODE": "proxy", "IO_AUDIT_LOG_FILE": "/proc/no-such-dir/audit.log"}, undone+":/undone")...)
	out, err := exec.Command("docker", append(args, image)...).CombinedOutput()
	if err == nil {
		t.Fatalf("the optimizer started without an audit log:\n%s", out)
	}
	if !strings.Contains(string(out), "audit log") {
		t.Errorf("the reason is not in the output:\n%s", out)
	}
	_ = u
}

// The proxy remembers the answer to an upload, to serve identical retries.
// That answer must not outlive the asset: a photo deleted from Immich and
// uploaded again has to be stored again.
func TestProxyUploadsAgainAfterTheAssetWasDeleted(t *testing.T) {
	u := newUser(t)
	undone := sharedDir(t)
	_, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")
	src := filepath.Join(mediaDir, "plain.jpg")
	fields := map[string]string{"deviceAssetId": "same-every-time"}

	status, out := u.upload(t, proxy, src, "plain.jpg", fields)
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, out["_body"])
	}
	first := u.waitReady(t, out["id"])

	// A retry while the asset exists gets the same answer.
	if status, out = u.upload(t, proxy, src, "plain.jpg", fields); out["id"] != first.ID {
		t.Fatalf("retry: %d %s, want asset %s again", status, out["_body"], first.ID)
	}

	must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{first.ID}, "force": true}, nil)
	eventually(t, time.Minute, "Immich to delete the asset", func() bool {
		return call(t, u.key, http.MethodGet, "/assets/"+first.ID, nil, nil) >= 400
	})
	status, out = u.upload(t, proxy, src, "plain.jpg", fields)
	if status != http.StatusCreated || out["id"] == first.ID || out["id"] == "" {
		t.Fatalf("upload after deletion: %d %s, want a new asset", status, out["_body"])
	}
	if a := u.waitReady(t, out["id"]); a.OriginalMimeType != "image/jxl" {
		t.Errorf("the new asset is %s, want the converted file", a.OriginalMimeType)
	}
}

// "downloads" in the tasks file: what is stored as JPEG XL is handed out as
// JPEG when its original is downloaded through the proxy.
func TestProxyConvertsDownloads(t *testing.T) {
	u := newUser(t)
	undone := sharedDir(t)
	base, err := os.ReadFile(filepath.Join(configDir, "lossless", "tasks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := sharedDir(t)
	downloads := `downloads:
  - name: jxl-to-jpeg
    command: djxl "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jpg"
    extensions: [jxl]
`
	if err := os.WriteFile(filepath.Join(cfg, "tasks.yaml"), []byte(downloads+string(base)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy", "IO_TASKS_FILE": "/custom/tasks.yaml"}, true,
		undone+":/undone", cfg+":/custom:ro")

	src := filepath.Join(mediaDir, "photo.jpg")
	status, out := u.upload(t, proxy, src, "photo.jpg", nil)
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, out["_body"])
	}
	jxl := u.waitReady(t, out["id"])
	if jxl.OriginalMimeType != "image/jxl" {
		t.Fatalf("stored as %s, want JPEG XL", jxl.OriginalMimeType)
	}
	status, out = u.upload(t, proxy, filepath.Join(mediaDir, "photo.heic"), "photo.heic", nil)
	if status != http.StatusCreated {
		t.Fatalf("upload: %d %s", status, out["_body"])
	}
	heic := u.waitReady(t, out["id"])

	get := func(base, id string, header map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+"/api/assets/"+id+"/original", nil)
		req.Header.Set(u.key[0], u.key[1])
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := httpc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	sum := func(b []byte) string {
		s := sha1.Sum(b)
		return hex.EncodeToString(s[:])
	}

	// Through the proxy: a JPEG, and since the task that made the JXL keeps
	// the JPEG's own data, the very file that was uploaded.
	resp, body := get(proxy, jxl.ID, nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("download: status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if d := resp.Header.Get("Content-Disposition"); !strings.Contains(d, "photo.jpg") {
		t.Errorf("Content-Disposition = %q, want the name photo.jpg", d)
	}
	if got, want := sum(body), fileSHA1(t, src); got != want {
		t.Errorf("the downloaded JPEG is not the uploaded one: %s, want %s (%d bytes)", got, want, len(body))
	}
	// A part of it, as a player or a download manager asks.
	part, partBody := get(proxy, jxl.ID, map[string]string{"Range": "bytes=10-19"})
	if part.StatusCode != http.StatusPartialContent || !bytes.Equal(partBody, body[10:20]) {
		t.Errorf("range request: status %d, %d bytes", part.StatusCode, len(partBody))
	}

	// Immich itself still has, and sends, the JXL.
	direct, directBody := get(immichURL, jxl.ID, nil)
	if direct.Header.Get("Content-Type") != "image/jxl" || sum(directBody) == sum(body) {
		t.Errorf("Immich's own download changed: type %q", direct.Header.Get("Content-Type"))
	}
	// A type no download task handles passes through byte for byte.
	_, viaProxy := get(proxy, heic.ID, nil)
	_, straight := get(immichURL, heic.ID, nil)
	if !bytes.Equal(viaProxy, straight) || len(straight) == 0 {
		t.Errorf("a file no download task handles was changed: %d bytes, want %d", len(viaProxy), len(straight))
	}
	// Immich's refusals stay Immich's.
	req, _ := http.NewRequest(http.MethodGet, proxy+"/api/assets/"+jxl.ID+"/original", nil)
	if r2, err := httpc.Do(req); err == nil {
		r2.Body.Close()
		if r2.StatusCode != http.StatusUnauthorized {
			t.Errorf("download without credentials: status %d, want 401", r2.StatusCode)
		}
	}
}
