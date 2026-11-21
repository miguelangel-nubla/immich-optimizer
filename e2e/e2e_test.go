//go:build e2e

// Package e2e runs the optimizer image against a real Immich server. Start it
// with scripts/e2e-test.sh, which brings Immich up with Docker Compose and
// sets the E2E_* variables these tests read.
//
// Every test creates its own Immich user, so tests cannot see each other's
// assets and the same sample files can be reused.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	immichURL   = os.Getenv("E2E_IMMICH_URL")
	internalURL = os.Getenv("E2E_IMMICH_INTERNAL_URL")
	network     = os.Getenv("E2E_NETWORK")
	image       = os.Getenv("E2E_IMAGE")
	binary      = os.Getenv("E2E_BINARY")
	configDir   = os.Getenv("E2E_CONFIG_DIR")
	mediaDir    = os.Getenv("E2E_MEDIA_DIR")

	httpc = &http.Client{Timeout: 2 * time.Minute}

	adminOnce  sync.Once
	adminToken string
	adminErr   error
)

const password = "e2e-password-1"

func TestMain(m *testing.M) {
	for name, v := range map[string]string{"E2E_IMMICH_URL": immichURL, "E2E_IMMICH_INTERNAL_URL": internalURL,
		"E2E_NETWORK": network, "E2E_IMAGE": image, "E2E_BINARY": binary, "E2E_CONFIG_DIR": configDir, "E2E_MEDIA_DIR": mediaDir} {
		if v == "" {
			fmt.Fprintf(os.Stderr, "%s is not set: run these tests with scripts/e2e-test.sh\n", name)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

// downloadsDir is where what the tests download is kept between runs:
// $IO_DEV_DIR/downloads when that is set (see scripts/dev-env.sh), and the
// system's cache folder otherwise.
func downloadsDir(t *testing.T) string {
	t.Helper()
	if dev := os.Getenv("IO_DEV_DIR"); dev != "" {
		return filepath.Join(dev, "downloads")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(cache, "immich-optimizer")
}

func randomName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// call sends a JSON request to Immich. auth is a header pair such as
// {"x-api-key", key}. It returns the status and decodes the answer into out.
func call(t *testing.T, auth [2]string, method, path string, body, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, immichURL+"/api"+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if auth[0] != "" {
		req.Header.Set(auth[0], auth[1])
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decoding %q: %v", method, path, data, err)
		}
	}
	if resp.StatusCode >= 300 && out != nil {
		t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

// must is call that also fails the test on a non-2xx answer.
func must(t *testing.T, auth [2]string, method, path string, body, out any) {
	t.Helper()
	if out == nil {
		var discard json.RawMessage
		out = &discard
	}
	call(t, auth, method, path, body, out)
}

type user struct {
	id   string
	key  [2]string // API key header
	keyS string
	// session is a login session header, as the mobile app uses.
	session [2]string
	email   string
}

func login(t *testing.T, email string) string {
	t.Helper()
	var out struct {
		AccessToken string `json:"accessToken"`
	}
	must(t, [2]string{}, http.MethodPost, "/auth/login", map[string]string{"email": email, "password": password}, &out)
	return out.AccessToken
}

func admin(t *testing.T) [2]string {
	t.Helper()
	adminOnce.Do(func() {
		// The first sign-up creates the admin; later runs just log in.
		call(t, [2]string{}, http.MethodPost, "/auth/admin-sign-up",
			map[string]string{"email": "admin@e2e.test", "password": password, "name": "Admin"}, nil)
		adminToken = login(t, "admin@e2e.test")
		if adminToken == "" {
			adminErr = fmt.Errorf("could not log in as admin")
		}
	})
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	return [2]string{"Authorization", "Bearer " + adminToken}
}

// newUser creates a fresh Immich user with an all-permissions API key.
func newUser(t *testing.T) *user {
	t.Helper()
	email := randomName() + "@e2e.test"
	var created struct {
		ID string `json:"id"`
	}
	must(t, admin(t), http.MethodPost, "/admin/users", map[string]any{"email": email, "password": password, "name": "E2E " + email,
		// Otherwise the mobile app stops at a "change password" page.
		"shouldChangePassword": false}, &created)
	token := [2]string{"Authorization", "Bearer " + login(t, email)}
	var key struct {
		Secret string `json:"secret"`
	}
	must(t, token, http.MethodPost, "/api-keys", map[string]any{"name": "e2e", "permissions": []string{"all"}}, &key)
	return &user{id: created.ID, key: [2]string{"x-api-key", key.Secret}, keyS: key.Secret, session: token, email: email}
}

type asset struct {
	ID               string  `json:"id"`
	OriginalFileName string  `json:"originalFileName"`
	OriginalMimeType string  `json:"originalMimeType"`
	Checksum         string  `json:"checksum"`
	FileCreatedAt    string  `json:"fileCreatedAt"`
	LocalDateTime    string  `json:"localDateTime"`
	IsFavorite       bool    `json:"isFavorite"`
	IsTrashed        bool    `json:"isTrashed"`
	IsEdited         bool    `json:"isEdited"`
	LivePhotoVideoID *string `json:"livePhotoVideoId"`
	Visibility       string  `json:"visibility"`
	Thumbhash        *string `json:"thumbhash"`
	ExifInfo         struct {
		Description      string   `json:"description"`
		Rating           *float64 `json:"rating"`
		DateTimeOriginal *string  `json:"dateTimeOriginal"`
		Latitude         *float64 `json:"latitude"`
		Model            *string  `json:"model"`
		Width            *float64 `json:"exifImageWidth"`
		Height           *float64 `json:"exifImageHeight"`
		Size             int64    `json:"fileSizeInByte"`
	} `json:"exifInfo"`
	Tags []struct {
		ID string `json:"id"`
	} `json:"tags"`
	People []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"people"`
	Stack *struct {
		ID             string `json:"id"`
		PrimaryAssetID string `json:"primaryAssetId"`
	} `json:"stack"`
}

func (u *user) asset(t *testing.T, id string) asset {
	t.Helper()
	var a asset
	must(t, u.key, http.MethodGet, "/assets/"+id, nil, &a)
	return a
}

// upload sends a file to url (Immich itself, or the proxy) and returns the
// status and the decoded answer.
func (u *user) upload(t *testing.T, base, path, name string, fields map[string]string) (int, map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	all := map[string]string{"deviceAssetId": name + randomName(), "deviceId": "e2e",
		"fileCreatedAt": "2019-03-04T05:06:07.000Z", "fileModifiedAt": "2019-03-04T05:06:07.000Z"}
	for k, v := range fields {
		all[k] = v
	}
	for k, v := range all {
		_ = mw.WriteField(k, v)
	}
	part, _ := mw.CreateFormFile("assetData", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/assets", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set(u.key[0], u.key[1])
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("upload of %s: %v", name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := map[string]string{}
	_ = json.Unmarshal(body, &out)
	out["_body"] = string(body)
	return resp.StatusCode, out
}

// add uploads a sample straight to Immich and waits until Immich has read it.
func (u *user) add(t *testing.T, sample string, fields map[string]string) asset {
	t.Helper()
	status, out := u.upload(t, immichURL, filepath.Join(mediaDir, sample), sample, fields)
	if status != http.StatusCreated {
		t.Fatalf("uploading %s: status %d: %s", sample, status, out["_body"])
	}
	return u.waitReady(t, out["id"])
}

func (u *user) waitReady(t *testing.T, id string) asset {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		a := u.asset(t, id)
		if a.Thumbhash != nil {
			return a
		}
		if time.Now().After(deadline) {
			t.Fatalf("Immich did not finish reading asset %s", id)
		}
		time.Sleep(time.Second)
	}
}

// live returns the user's assets that are not in the trash.
func (u *user) live(t *testing.T) []asset {
	t.Helper()
	var out struct {
		Assets struct {
			Items []asset `json:"items"`
		} `json:"assets"`
	}
	must(t, u.key, http.MethodPost, "/search/metadata", map[string]any{"size": 500, "withExif": true}, &out)
	return out.Assets.Items
}

// marker returns the asset's optimizer marker, or nil. The record is nested;
// here its fields are addressed by their path, such as "original.sha1" (the
// first version) or "latest.task.name" (the last), or "versions.1.sha1".
func (u *user) marker(t *testing.T, id string) map[string]any {
	t.Helper()
	var items []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	}
	must(t, u.key, http.MethodGet, "/assets/"+id+"/metadata", nil, &items)
	for _, it := range items {
		if it.Key == "immich-optimizer" {
			flat := map[string]any{}
			var walk func(prefix string, v map[string]any)
			walk = func(prefix string, v map[string]any) {
				for k, x := range v {
					if sub, ok := x.(map[string]any); ok {
						walk(prefix+k+".", sub)
						continue
					}
					flat[prefix+k] = x
				}
			}
			walk("", it.Value)
			// The versions of the file, oldest first: the original is
			// the first, the file as it is now the last.
			versions, _ := it.Value["versions"].([]any)
			flat["versions.count"] = float64(len(versions))
			for i, v := range versions {
				if vm, ok := v.(map[string]any); ok {
					walk(fmt.Sprintf("versions.%d.", i), vm)
					if i == 0 {
						walk("original.", vm)
					}
					if i == len(versions)-1 && i > 0 {
						walk("latest.", vm)
					}
				}
			}
			return flat
		}
	}
	return nil
}

func (u *user) albumsOf(t *testing.T, id string) []string {
	t.Helper()
	var out []struct {
		ID string `json:"id"`
	}
	must(t, u.key, http.MethodGet, "/albums?assetId="+id, nil, &out)
	var ids []string
	for _, a := range out {
		ids = append(ids, a.ID)
	}
	return ids
}

func (u *user) linkAssets(t *testing.T, linkID string) []string {
	t.Helper()
	var out struct {
		Assets []struct {
			ID string `json:"id"`
		} `json:"assets"`
	}
	must(t, u.key, http.MethodGet, "/shared-links/"+linkID, nil, &out)
	var ids []string
	for _, a := range out.Assets {
		ids = append(ids, a.ID)
	}
	return ids
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func hexChecksum(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func fileSHA1(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := sha1.Sum(data)
	return hex.EncodeToString(s[:])
}

// sharedDir makes a directory the container's user can write to.
func sharedDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "io-e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "run", "--rm", "-u", "0", "-v", dir+":/d", "--entrypoint", "sh", image, "-c", "rm -rf /d/* /d/.[!.]*").Run()
		os.RemoveAll(dir)
	})
	return dir
}

// dockerArgs are the arguments every optimizer container gets: this
// checkout's binary and profiles, on Immich's network.
func dockerArgs(env map[string]string, mounts ...string) []string {
	args := []string{"--network", network,
		"-v", binary + ":/usr/local/bin/immich-optimizer:ro",
		"-v", configDir + ":/cfg:ro",
		"-e", "IO_IMMICH_URL=" + internalURL,
	}
	if _, ok := env["IO_TASKS_FILE"]; !ok {
		args = append(args, "-e", "IO_TASKS_FILE=/cfg/lossless/tasks.yaml")
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	for _, m := range mounts {
		// A mount, or any other docker option written out in full.
		if strings.HasPrefix(m, "--") {
			args = append(args, m)
			continue
		}
		args = append(args, "-v", m)
	}
	return args
}

// The optimizer's tags, as library mode names them.
const (
	tagConvert = "immich-optimizer/convert"
	tagKeep    = "immich-optimizer/keep"
	tagFailed  = "immich-optimizer/failed"
)

// tagID returns the id of the user's tag with this value, creating it.
func (u *user) tagID(t *testing.T, value string) string {
	t.Helper()
	var out []struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	must(t, u.key, http.MethodPut, "/tags", map[string]any{"tags": []string{value}}, &out)
	for _, tg := range out {
		if tg.Value == value {
			return tg.ID
		}
	}
	t.Fatalf("Immich did not return the tag %s: %+v", value, out)
	return ""
}

// tag gives assets a tag, in Immich, as a person would.
func (u *user) tag(t *testing.T, value string, ids ...string) {
	t.Helper()
	u.tagWith(t, []string{value}, ids...)
}

// tagWith gives assets several tags in one call, as Immich's interface does.
// Separate calls in quick succession on the same asset are not safe: Immich
// mirrors each change into the asset's metadata and sidecar in the
// background, and a change can be lost in between.
func (u *user) tagWith(t *testing.T, values []string, ids ...string) {
	t.Helper()
	var tagIDs []string
	for _, v := range values {
		tagIDs = append(tagIDs, u.tagID(t, v))
	}
	must(t, u.key, http.MethodPut, "/tags/assets", map[string]any{"tagIds": tagIDs, "assetIds": ids}, nil)
}

// untag removes a tag from an asset, as a person would, and waits until
// Immich has applied it: Immich can lose one of two quick changes.
func (u *user) untag(t *testing.T, value, id string) {
	t.Helper()
	must(t, u.key, http.MethodDelete, "/tags/"+u.tagID(t, value)+"/assets", map[string]any{"ids": []string{id}}, nil)
	eventually(t, time.Minute, value+" to be removed", func() bool {
		time.Sleep(2 * time.Second)
		return !contains(u.tags(t, id), value)
	})
}

// tags returns the values of an asset's tags.
func (u *user) tags(t *testing.T, id string) []string {
	t.Helper()
	var a struct {
		Tags []struct {
			Value string `json:"value"`
		} `json:"tags"`
	}
	must(t, u.key, http.MethodGet, "/assets/"+id, nil, &a)
	var out []string
	for _, tg := range a.Tags {
		out = append(out, tg.Value)
	}
	return out
}

// runLibrary starts library mode, waits until it has done what it does at
// startup (settle what is unfinished, act on what is tagged), stops it, and
// returns its exit code, its output and the parsed audit log.
//
// Library mode converts only what is tagged immich-optimizer/convert. Unless
// env has "E2E_TAGS": "own", every asset the user has is tagged so before
// starting, which is what the tests about converting itself want; the test of
// the tags tags its assets itself.
func runLibrary(t *testing.T, u *user, logDir string, env map[string]string, mounts ...string) (int, string, []map[string]any) {
	t.Helper()
	all := map[string]string{"IO_MODE": "library", "IO_IMMICH_API_KEY": u.keyS, "IO_LIBRARY_READY_TIMEOUT": "2m"}
	for k, v := range env {
		all[k] = v
	}
	if all["E2E_TAGS"] != "own" {
		var ids []string
		for _, a := range u.live(t) {
			ids = append(ids, a.ID)
		}
		if len(ids) > 0 {
			u.tag(t, tagConvert, ids...)
		}
	}
	delete(all, "E2E_TAGS")
	name := "io-e2e-lib-" + randomName()
	args := append([]string{"run", "-d", "--name", name}, dockerArgs(all, append(mounts, logDir+":/undone")...)...)
	if out, err := exec.Command("docker", append(args, image)...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	defer func() { _ = exec.Command("docker", "rm", "-f", name).Run() }()
	waitLibraryReady(t, name, 10*time.Minute)
	_ = exec.Command("docker", "stop", "-t", "60", name).Run()
	codeOut, err := exec.Command("docker", "inspect", "-f", "{{.State.ExitCode}}", name).Output()
	if err != nil {
		t.Fatalf("docker inspect: %v", err)
	}
	code, _ := strconv.Atoi(strings.TrimSpace(string(codeOut)))
	out, _ := exec.Command("docker", "logs", name).CombinedOutput()
	return code, string(out), readLog(t, filepath.Join(logDir, "audit.log"))
}

// libraryReady is what library mode logs once it has done its startup work
// and waits for tags.
const libraryReady = "Library: waiting for tags"

// waitLibraryReady waits until the library container has done its startup
// work, or has stopped.
func waitLibraryReady(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		if strings.Contains(string(out), libraryReady) {
			return
		}
		running, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name).Output()
		if strings.TrimSpace(string(running)) != "true" {
			return
		}
		time.Sleep(time.Second)
	}
	out, _ := exec.Command("docker", "logs", "--tail", "40", name).CombinedOutput()
	t.Fatalf("library mode did not finish starting in %s:\n%s", timeout, out)
}

func readLog(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		entries = append(entries, e)
	}
	return entries
}

// events returns the event names logged for one original asset ("" = all).
func events(entries []map[string]any, assetID string) []string {
	var out []string
	for _, e := range entries {
		if assetID == "" || e["asset"] == assetID {
			out = append(out, fmt.Sprint(e["event"]))
		}
	}
	return out
}

// startOptimizer runs a long-lived optimizer container (proxy or watcher) and
// returns its name and, for the proxy, the host URL it is published on.
func startOptimizer(t *testing.T, env map[string]string, publish bool, mounts ...string) (name, url string) {
	t.Helper()
	return startOptimizerOn(t, env, publish, "", mounts...)
}

// startOptimizerOn is startOptimizer with the proxy on a given host port
// ("" for any free one).
func startOptimizerOn(t *testing.T, env map[string]string, publish bool, port string, mounts ...string) (name, url string) {
	t.Helper()
	name = "io-e2e-" + randomName()
	args := []string{"run", "-d", "--name", name}
	if publish {
		args = append(args, "-p", "127.0.0.1:"+port+":2283")
	}
	args = append(args, dockerArgs(env, mounts...)...)
	if out, err := exec.Command("docker", append(args, image)...).CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "logs", "--tail", "60", name).CombinedOutput()
			t.Logf("optimizer logs:\n%s", logs)
		}
		_ = exec.Command("docker", "rm", "-f", name).Run()
	})
	if !publish {
		return name, ""
	}
	out, err := exec.Command("docker", "port", name, "2283/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	url = "http://" + addr
	deadline := time.Now().Add(time.Minute)
	for {
		resp, err := httpc.Get(url + "/api/server/ping")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return name, url
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy did not come up on %s", url)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
