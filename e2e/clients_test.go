//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Clients other than the web and mobile apps, through the proxy. The versions
// are pinned: what is tested is the proxy, against clients that are known.
const (
	cliImage        = "ghcr.io/immich-app/immich-cli:3.4.2"
	immichGoVersion = "v0.32.0"
)

// converted checks that the user has exactly n assets and that each is a
// converted file with a marker.
func (u *user) converted(t *testing.T, n int) {
	t.Helper()
	live := u.live(t)
	if len(live) != n {
		t.Fatalf("Immich has %d assets, want %d", len(live), n)
	}
	for _, a := range live {
		if a.OriginalMimeType != "image/jxl" || !strings.HasSuffix(a.OriginalFileName, ".jxl") {
			t.Errorf("%s is %s, want a converted file", a.OriginalFileName, a.OriginalMimeType)
		}
		if u.marker(t, a.ID) == nil {
			t.Errorf("%s has no marker", a.OriginalFileName)
		}
	}
}

// importDir is a folder with the given samples, to hand to a client.
func importDir(t *testing.T, samples ...string) string {
	t.Helper()
	dir := sharedDir(t)
	for _, s := range samples {
		data, err := os.ReadFile(filepath.Join(mediaDir, s))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, s), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Immich's own command line client.
func TestProxyImmichCLI(t *testing.T) {
	u := newUser(t)
	undone := sharedDir(t)
	name, _ := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")
	dir := importDir(t, "photo.jpg", "plain.jpg")

	upload := func() string {
		t.Helper()
		out, err := exec.Command("docker", "run", "--rm", "--network", network,
			"-e", "IMMICH_INSTANCE_URL=http://"+name+":2283/api", "-e", "IMMICH_API_KEY="+u.keyS,
			"-v", dir+":/import:ro", cliImage, "upload", "--recursive", "/import").CombinedOutput()
		if err != nil {
			t.Fatalf("immich upload: %v\n%s", err, out)
		}
		return string(out)
	}
	out := upload()
	for _, a := range u.live(t) {
		u.waitReady(t, a.ID)
	}
	u.converted(t, 2)
	if n := received(t, name); n != 2 {
		t.Errorf("the proxy received %d uploads, want 2:\n%s", n, out)
	}

	// The client checks by checksum what the server already has. It does
	// not find the converted files, so it sends everything again; nothing
	// may be stored twice.
	upload()
	u.converted(t, 2)
}

// immichGo downloads the pinned immich-go for the Docker host's platform and
// returns the folder it is in.
func immichGo(t *testing.T) string {
	t.Helper()
	arch := map[string]string{"amd64": "x86_64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		t.Skipf("no immich-go build for %s", runtime.GOARCH)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, "immich-optimizer", "immich-go-"+immichGoVersion+"-"+arch)
	bin := filepath.Join(dir, "immich-go")
	if _, err := os.Stat(bin); err == nil {
		return dir
	}
	url := fmt.Sprintf("https://github.com/simulot/immich-go/releases/download/%s/immich-go_Linux_%s.tar.gz", immichGoVersion, arch)
	resp, err := httpc.Get(url)
	if err != nil {
		t.Fatalf("downloading immich-go: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("downloading immich-go: status %d for %s", resp.StatusCode, url)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			t.Fatalf("immich-go is not in the archive: %v", err)
		}
		if filepath.Base(h.Name) != "immich-go" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(bin+".part", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := os.Rename(bin+".part", bin); err != nil {
			t.Fatal(err)
		}
		return dir
	}
}

// immich-go, the most used third-party bulk uploader.
func TestProxyImmichGo(t *testing.T) {
	tool := immichGo(t)
	u := newUser(t)
	undone := sharedDir(t)
	name, _ := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")
	dir := importDir(t, "photo.jpg", "plain.jpg")

	upload := func() string {
		t.Helper()
		// It is a static binary: any Linux image runs it.
		out, err := exec.Command("docker", "run", "--rm", "--network", network,
			"-v", tool+":/tool:ro", "-v", dir+":/import:ro", "-w", "/tmp", "-e", "HOME=/tmp", "--entrypoint", "/tool/immich-go", image,
			"upload", "from-folder", "--server=http://"+name+":2283", "--api-key="+u.keyS,
			"--no-ui", "--pause-immich-jobs=false", "/import").CombinedOutput()
		if err != nil {
			t.Fatalf("immich-go: %v\n%s", err, out)
		}
		return string(out)
	}
	out := upload()
	for _, a := range u.live(t) {
		u.waitReady(t, a.ID)
	}
	u.converted(t, 2)
	if n := received(t, name); n != 2 {
		t.Errorf("the proxy received %d uploads, want 2:\n%s", n, out)
	}
	upload()
	u.converted(t, 2)
}

// An upload into a shared album by someone who only has the link: the key is
// in the address, and there are no credentials.
func TestProxySharedLinkUpload(t *testing.T) {
	u := newUser(t)
	undone := sharedDir(t)
	_, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")

	var album struct {
		ID string `json:"id"`
	}
	must(t, u.key, http.MethodPost, "/albums", map[string]any{"albumName": "shared"}, &album)
	var link struct {
		Key string `json:"key"`
	}
	must(t, u.key, http.MethodPost, "/shared-links", map[string]any{"type": "ALBUM", "albumId": album.ID, "allowUpload": true}, &link)

	data, err := os.ReadFile(filepath.Join(mediaDir, "photo.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range map[string]string{"deviceAssetId": "guest-1", "deviceId": "guest",
		"fileCreatedAt": "2019-03-04T05:06:07.000Z", "fileModifiedAt": "2019-03-04T05:06:07.000Z"} {
		_ = mw.WriteField(k, v)
	}
	part, _ := mw.CreateFormFile("assetData", "photo.jpg")
	_, _ = part.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, proxy+"/api/assets?key="+link.Key, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload with a shared link key: status %d: %s", resp.StatusCode, body)
	}
	for _, a := range u.live(t) {
		u.waitReady(t, a.ID)
	}
	u.converted(t, 1)
	// Without the key the same upload is refused, by Immich.
	req, _ = http.NewRequest(http.MethodPost, proxy+"/api/assets", bytes.NewReader(nil))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if resp, err = httpc.Do(req); err == nil {
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("an upload without credentials was accepted: %d", resp.StatusCode)
		}
	}
}
