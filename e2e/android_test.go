//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// These tests drive the real Immich Android app in an emulator, which is the
// only way to know what the app makes of what the proxy sends it. They run
// with E2E_ANDROID=1 scripts/e2e-test.sh, which boots the emulator with
// scripts/android-emulator.sh and sets the two variables below.
var (
	androidSerial = os.Getenv("E2E_ANDROID_SERIAL")
	adbPath       = os.Getenv("E2E_ADB")
)

const (
	immichApp = "app.alextran.immich"
	appDB     = "/data/data/" + immichApp + "/app_flutter/immich.sqlite"
	// hiddenLibrary is the library id the proxy gives its hidden entries.
	hiddenLibrary = "696d6d69-6368-4f70-8074-696d697a6572"
)

// phone is the emulator. All of it goes through adb.
type phone struct{ t *testing.T }

func (p phone) adb(args ...string) string {
	p.t.Helper()
	out, err := exec.Command(adbPath, append([]string{"-s", androidSerial}, args...)...).CombinedOutput()
	if err != nil {
		p.t.Fatalf("adb %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (p phone) shell(cmd string) string {
	p.t.Helper()
	return p.adb("shell", cmd)
}

// node is one element of the screen, as Android's accessibility tree has it.
type node struct {
	Class   string `xml:"class,attr"`
	Text    string `xml:"text,attr"`
	Desc    string `xml:"content-desc,attr"`
	Bounds  string `xml:"bounds,attr"`
	Checked string `xml:"checked,attr"`
	Nodes   []node `xml:"node"`
}

var boundsRe = regexp.MustCompile(`\d+`)

// box returns the node's left, top, right and bottom.
func (n node) box() (x1, y1, x2, y2 int) {
	m := boundsRe.FindAllString(n.Bounds, -1)
	if len(m) != 4 {
		return
	}
	x1, _ = strconv.Atoi(m[0])
	y1, _ = strconv.Atoi(m[1])
	x2, _ = strconv.Atoi(m[2])
	y2, _ = strconv.Atoi(m[3])
	return
}

// label is what the node says, on one line.
func (n node) label() string {
	return strings.ReplaceAll(n.Desc, "\n", " / ")
}

// screen returns the elements on screen. It returns nil while the screen is
// changing, which is when Android cannot read it.
func (p phone) screen() []node {
	_, _ = exec.Command(adbPath, "-s", androidSerial, "shell", "uiautomator dump /sdcard/io-ui.xml").CombinedOutput()
	out, err := exec.Command(adbPath, "-s", androidSerial, "shell", "cat /sdcard/io-ui.xml").Output()
	if err != nil {
		return nil
	}
	var root struct {
		Nodes []node `xml:"node"`
	}
	if xml.Unmarshal(out, &root) != nil {
		return nil
	}
	var flat []node
	var walk func(ns []node)
	walk = func(ns []node) {
		for _, n := range ns {
			flat = append(flat, n)
			walk(n.Nodes)
		}
	}
	walk(root.Nodes)
	return flat
}

// find returns the first element of the given class ("" for any) whose label
// starts with prefix.
func find(nodes []node, class, prefix string) (node, bool) {
	for _, n := range nodes {
		if (class == "" || strings.HasSuffix(n.Class, "."+class)) && strings.HasPrefix(n.label(), prefix) {
			return n, true
		}
	}
	return node{}, false
}

// wait returns the element once it is on screen.
func (p phone) wait(class, prefix string) node {
	p.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	var last []node
	for {
		last = p.screen()
		if n, ok := find(last, class, prefix); ok {
			return n
		}
		if time.Now().After(deadline) {
			var seen []string
			for _, n := range last {
				if n.Desc != "" || n.Text != "" {
					seen = append(seen, fmt.Sprintf("%s %q %q", n.Class, n.label(), n.Text))
				}
			}
			p.t.Fatalf("%s %q did not appear in the app; on screen:\n%s", class, prefix, strings.Join(seen, "\n"))
		}
		time.Sleep(time.Second)
	}
}

func (p phone) tapAt(x, y int) {
	p.t.Helper()
	p.shell(fmt.Sprintf("input tap %d %d", x, y))
}

// tap waits for the element and taps its centre.
func (p phone) tap(class, prefix string) {
	p.t.Helper()
	x1, y1, x2, y2 := p.wait(class, prefix).box()
	p.tapAt((x1+x2)/2, (y1+y2)/2)
}

// fields returns the text fields on screen, top to bottom.
func (p phone) fields(n int) []node {
	p.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var out []node
		for _, e := range p.screen() {
			if strings.HasSuffix(e.Class, ".EditText") {
				out = append(out, e)
			}
		}
		if len(out) == n {
			return out
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("expected %d text field(s) in the app, found %d", n, len(out))
		}
		time.Sleep(time.Second)
	}
}

// typeInto fills the i-th of n text fields and checks what ended up in it.
// It is retried: while the keyboard is still opening the app can drop what is
// typed, or keep only a part of it.
func (p phone) typeInto(i, n int, text string) {
	p.t.Helper()
	// A password field shows a dot for every character.
	typed := func(got string) bool {
		return got == text || utf8.RuneCountInString(got) == utf8.RuneCountInString(text) && strings.Trim(got, "\u2022*") == ""
	}
	for try := 0; try < 6; try++ {
		f := p.fields(n)[i]
		if typed(f.Text) {
			return
		}
		x1, y1, x2, y2 := f.box()
		p.tapAt((x1+x2)/2, (y1+y2)/2)
		time.Sleep(2 * time.Second)
		if f.Text != "" {
			// Something partial is in it: select all of it and delete.
			p.shell("input keycombination 113 29")
			p.shell("input keyevent 67")
			time.Sleep(time.Second)
		}
		p.shell("input text '" + text + "'")
		time.Sleep(time.Second)
	}
	if got := p.fields(n)[i].Text; !typed(got) {
		p.t.Fatalf("could not type into text field %d: it holds %q", i, got)
	}
}

func (p phone) launch() {
	p.t.Helper()
	p.shell("monkey -p " + immichApp + " -c android.intent.category.LAUNCHER 1")
}

func (p phone) restart() {
	p.t.Helper()
	p.shell("am force-stop " + immichApp)
	p.launch()
	p.wait("Button", "Backup")
}

// queryInt runs a counting query on the app's own database; it returns -1 while the database is busy or
// not there yet.
func (p phone) queryInt(sql string) int {
	out, err := exec.Command(adbPath, "-s", androidSerial, "shell", `sqlite3 -readonly `+appDB+` "`+sql+`"`).Output()
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return n
}

const (
	// sqlNotBackedUp counts the phone's photos the app has no server copy
	// for: what it will upload.
	sqlNotBackedUp = "select count(*) from local_asset_entity l where not exists (select 1 from remote_asset_entity r where r.checksum = l.checksum)"
	sqlRemote      = "select count(*) from remote_asset_entity where library_id is null"
	sqlHidden      = "select count(*) from remote_asset_entity where library_id = '" + hiddenLibrary + "'"
)

// eventually polls until check passes.
func eventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Second)
	}
}

// setUpPhone gives the app a clean start with the given photos in one album,
// logs it in to the proxy as u and switches backup on for that album.
func setUpPhone(t *testing.T, proxyURL string, u *user, samples []string) phone {
	t.Helper()
	p := phone{t}
	album := "io" + randomName()
	dir := "/sdcard/DCIM/" + album
	// The emulator is only for these tests: the app must find this album and
	// nothing else, so every photo on it goes.
	wipe := func() {
		_, _ = exec.Command(adbPath, "-s", androidSerial, "shell",
			"am force-stop "+immichApp+"; rm -rf /sdcard/DCIM/* /sdcard/Pictures/*"+
				"; content delete --uri content://media/external/images/media"+
				"; content delete --uri content://media/external/video/media").CombinedOutput()
	}
	wipe()
	t.Cleanup(func() {
		if t.Failed() {
			// What the app knew when the test gave up.
			out, _ := exec.Command(adbPath, "-s", androidSerial, "shell", `sqlite3 -readonly `+appDB+
				` "select 'local', name, checksum from local_asset_entity" "select 'remote', id, name, checksum, visibility, library_id, deleted_at from remote_asset_entity"`).CombinedOutput()
			t.Logf("the app's database:\n%s", out)
		}
		wipe()
	})

	// A fresh app: no account, no database. Clearing also takes its
	// permissions away, so they are given back.
	p.shell("am force-stop " + immichApp)
	p.shell("pm clear " + immichApp)
	for _, perm := range []string{"READ_MEDIA_IMAGES", "READ_MEDIA_VIDEO", "ACCESS_MEDIA_LOCATION", "POST_NOTIFICATIONS"} {
		p.shell("pm grant " + immichApp + " android.permission." + perm)
	}

	p.shell("mkdir -p " + dir)
	for _, s := range samples {
		p.adb("push", filepath.Join(mediaDir, s), dir+"/"+s)
		p.shell("am broadcast -a android.intent.action.MEDIA_SCANNER_SCAN_FILE -d file://" + dir + "/" + s)
	}

	// The emulator reaches the host's loopback interface as 10.0.2.2.
	port := proxyURL[strings.LastIndex(proxyURL, ":")+1:]
	p.launch()
	p.wait("Button", "Next")
	p.typeInto(0, 1, "http://10.0.2.2:"+port)
	p.tap("Button", "Next")
	p.wait("Button", "Login")
	p.typeInto(0, 2, u.email)
	p.typeInto(1, 2, password)
	p.tap("Button", "Login")
	p.wait("Button", "Backup")

	// The app lists the phone's albums when it starts; one more start makes
	// sure the new album is in.
	p.restart()
	p.tap("Button", "Backup")
	p.tap("Button", "Select")
	p.tap("Button", album)
	time.Sleep(time.Second)
	p.shell("input keyevent KEYCODE_BACK")
	p.wait("", "Remainder")
	sw := p.wait("Switch", "Enable Backup")
	_, y1, x2, y2 := sw.box()
	p.tapAt(x2-150, (y1+y2)/2)
	eventually(t, 30*time.Second, "backup to be switched on in the app", func() bool {
		n, ok := find(p.screen(), "Switch", "Enable Backup")
		return ok && n.Checked == "true"
	})
	return p
}

func sha1Base64(t *testing.T, path string) string {
	t.Helper()
	raw, err := hex.DecodeString(fileSHA1(t, path))
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// received is how many uploads the app has sent to the proxy. The audit log
// does not tell: a repeated upload is answered from the proxy's memory.
func received(t *testing.T, container string) int {
	t.Helper()
	out, err := exec.Command("docker", "logs", container).CombinedOutput()
	if err != nil {
		t.Fatalf("docker logs: %v", err)
	}
	return strings.Count(string(out), "Intercepted upload request")
}

func uploads(t *testing.T, undone string) int {
	t.Helper()
	n := 0
	for _, e := range readLog(t, filepath.Join(undone, "audit.log")) {
		if e["event"] == "uploaded" {
			n++
		}
	}
	return n
}

// assetOf returns the user's asset that was converted from the sample.
func (u *user) assetOf(t *testing.T, sample string) string {
	t.Helper()
	sum := fileSHA1(t, filepath.Join(mediaDir, sample))
	for _, a := range u.live(t) {
		if m := u.marker(t, a.ID); m != nil && m["sha1"] == sum {
			return a.ID
		}
	}
	t.Fatalf("no asset in Immich was converted from %s", sample)
	return ""
}

// The reason the experimental app sync exists: the app decides what to back
// up by comparing the phone's files with the checksums the server reports,
// and a converted photo has a different one. These run backups in the real
// app, with the setting off and on.
func TestAndroidAppBackup(t *testing.T) {
	if androidSerial == "" || adbPath == "" {
		t.Skip("needs an Android emulator: run with E2E_ANDROID=1 scripts/e2e-test.sh")
	}
	photos := []string{"photo.jpg", "plain.jpg", "photo.png"}

	t.Run("without app sync the phone uploads converted photos again", func(t *testing.T) {
		u := newUser(t)
		undone := sharedDir(t)
		name, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")
		p := setUpPhone(t, proxy, u, photos)

		eventually(t, 3*time.Minute, "the app to upload the photos", func() bool { return uploads(t, undone) >= len(photos) })
		eventually(t, 2*time.Minute, "the app to learn about the converted assets", func() bool { return p.queryInt(sqlRemote) == len(photos) })
		if n := p.queryInt(sqlNotBackedUp); n != len(photos) {
			t.Fatalf("the app has %d photo(s) left to back up, want all %d: the converted checksums should not match", n, len(photos))
		}
		// So when it looks again, it sends them all again.
		p.restart()
		eventually(t, 2*time.Minute, "the app to upload the photos a second time", func() bool { return received(t, name) >= 2*len(photos) })
	})

	t.Run("with app sync the phone sees them as backed up and uploads once", func(t *testing.T) {
		u := newUser(t)
		undone := sharedDir(t)
		name, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy", "IO_PROXY_EXPERIMENTAL_APP_SYNC": "true"}, true, undone+":/undone")
		// A motion photo (converted without its video) and a video (not
		// converted by this profile) next to the ordinary photos.
		samples := append(append([]string(nil), photos...), "motion.jpg", "clip.mp4")
		converted := len(samples) - 1
		p := setUpPhone(t, proxy, u, samples)

		checksum := func(sample string) string { return sha1Base64(t, filepath.Join(mediaDir, sample)) }
		hidden := func(sample, more string) int {
			return p.queryInt("select count(*) from remote_asset_entity where visibility = 1 and library_id = '" + hiddenLibrary +
				"' and checksum = '" + checksum(sample) + "'" + more)
		}
		// freeable is the app's own test for "Free up space": a photo on the
		// phone whose checksum the server has, outside the trash.
		freeable := func(sample string) int {
			return p.queryInt("select count(*) from local_asset_entity l join remote_asset_entity r on r.checksum = l.checksum" +
				" where r.deleted_at is null and l.name = '" + sample + "'")
		}

		eventually(t, 3*time.Minute, "the app to upload everything", func() bool { return received(t, name) >= len(samples) })
		// Without the app being restarted: the hidden entries come over the
		// websocket, right behind Immich's own message about each upload.
		eventually(t, 3*time.Minute, "the hidden entries to reach the app", func() bool { return p.queryInt(sqlHidden) == converted })
		eventually(t, time.Minute, "the app to count everything as backed up", func() bool { return p.queryInt(sqlNotBackedUp) == 0 })
		for _, s := range samples {
			want := 1
			if s == "clip.mp4" {
				want = 0 // stored as it is: its own entry has the right checksum
			}
			if n := hidden(s, ""); n != want {
				t.Errorf("%s: %d hidden entries with the original's checksum, want %d", s, n, want)
			}
			if n := freeable(s); n != 1 {
				t.Errorf("%s: the app would not offer to free it up (%d)", s, n)
			}
		}

		// Starting the app again makes it sync and look for things to back
		// up. Nothing may be uploaded a second time.
		for i := 0; i < 2; i++ {
			p.restart()
			time.Sleep(15 * time.Second)
		}
		if n := received(t, name); n != len(samples) {
			t.Fatalf("the proxy received %d uploads, want %d: the app uploaded again", n, len(samples))
		}
		if n := p.queryInt(sqlNotBackedUp); n != 0 {
			t.Fatalf("after restarting, the app has %d file(s) left to back up, want 0", n)
		}
		if n := p.queryInt(sqlHidden); n != converted {
			t.Fatalf("after restarting, the app has %d hidden entries, want %d", n, converted)
		}
		if live := u.live(t); len(live) != len(samples) {
			t.Fatalf("Immich has %d assets, want %d", len(live), len(samples))
		}

		// What the user sees on the backup page.
		p.tap("Button", "Backup")
		if got := p.wait("", "Remainder").label(); !strings.Contains(got, "/ 0 /") {
			t.Fatalf("the app's backup page says %q, want a remainder of 0", got)
		}
		p.shell("input keyevent KEYCODE_BACK")

		// Trashing the converted asset on the server: the phone must not
		// consider its photo safe to delete any more, and must not upload it
		// again either.
		id := u.assetOf(t, "photo.jpg")
		must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{id}}, nil)
		eventuallyRestarting(t, p, 3*time.Minute, "the hidden entry to follow its asset into the trash", func() bool {
			return hidden("photo.jpg", " and deleted_at is not null") == 1
		})
		if n := freeable("photo.jpg"); n != 0 {
			t.Fatalf("the app would free up a photo whose only server copy is in the trash (%d)", n)
		}
		if n := freeable("plain.jpg"); n != 1 {
			t.Fatalf("trashing one asset changed another (%d)", n)
		}
		p.restart()
		time.Sleep(15 * time.Second)
		if n := received(t, name); n != len(samples) {
			t.Fatalf("the proxy received %d uploads, want %d: the app uploaded a trashed photo again", n, len(samples))
		}

		// Restoring it brings everything back.
		must(t, u.key, http.MethodPost, "/trash/restore/assets", map[string]any{"ids": []string{id}}, nil)
		eventuallyRestarting(t, p, 3*time.Minute, "the hidden entry to follow its asset out of the trash", func() bool {
			return hidden("photo.jpg", " and deleted_at is null") == 1
		})
		if n := freeable("photo.jpg"); n != 1 {
			t.Fatalf("after restoring, the app would not offer to free the photo up (%d)", n)
		}

		// Deleting it for good: the photo is still on the phone, so the app
		// backs it up again, as it does with any asset deleted on the server.
		must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{id}, "force": true}, nil)
		eventuallyRestarting(t, p, 4*time.Minute, "the app to upload the deleted photo again", func() bool {
			return received(t, name) == len(samples)+1
		})
		eventuallyRestarting(t, p, 3*time.Minute, "the new upload to count as backed up", func() bool {
			return hidden("photo.jpg", " and deleted_at is null") == 1 && p.queryInt(sqlNotBackedUp) == 0 && p.queryInt(sqlHidden) == converted
		})
		if id2 := u.assetOf(t, "photo.jpg"); id2 == id {
			t.Fatalf("the deleted asset %s is still there", id)
		}
	})

	t.Run("turning app sync on for a phone that had already synced", func(t *testing.T) {
		u := newUser(t)
		undone := sharedDir(t)
		port := freePort(t)
		first, proxy := startOptimizerOn(t, map[string]string{"IO_MODE": "proxy"}, true, port, undone+":/undone")
		p := setUpPhone(t, proxy, u, photos)
		eventually(t, 3*time.Minute, "the app to upload the photos", func() bool { return received(t, first) >= len(photos) })
		eventually(t, 2*time.Minute, "the app to learn about the converted assets", func() bool { return p.queryInt(sqlRemote) == len(photos) })

		// The same proxy address, now with the setting on.
		if out, err := exec.Command("docker", "rm", "-f", first).CombinedOutput(); err != nil {
			t.Fatalf("docker rm: %v\n%s", err, out)
		}
		second, _ := startOptimizerOn(t, map[string]string{"IO_MODE": "proxy", "IO_PROXY_EXPERIMENTAL_APP_SYNC": "true"}, true, port, undone+":/undone")

		// The app had synced the markers before the setting was on. It gets
		// the hidden entries when Immich next sends it one of those assets,
		// or, when that does not happen, after it has uploaded the photos
		// once more: the proxy finds the conversions already in Immich and
		// announces their markers again (TestProxyAppSyncHealsAndFollowsTheTrash
		// covers that path on its own).
		eventuallyRestarting(t, p, 5*time.Minute, "the hidden entries to reach the app", func() bool {
			return p.queryInt(sqlHidden) == len(photos) && p.queryInt(sqlNotBackedUp) == 0
		})
		settled := received(t, second)
		for i := 0; i < 2; i++ {
			p.restart()
			time.Sleep(15 * time.Second)
		}
		if n := received(t, second); n != settled {
			t.Fatalf("the app kept uploading: %d uploads after it had settled at %d", n, settled)
		}
		if live := u.live(t); len(live) != len(photos) {
			t.Fatalf("Immich has %d assets, want %d", len(live), len(photos))
		}
	})

	// Library mode converts what is already in Immich and moves the
	// originals to the trash. A phone that still has those photos would
	// upload them again once the trash is emptied, unless it learns the
	// originals' checksums from the conversions.
	t.Run("library mode on photos that are still on the phone", func(t *testing.T) {
		u := newUser(t)
		undone := sharedDir(t)
		// The photos are in Immich already, as uploaded before there was a proxy.
		var originals []string
		for _, s := range photos {
			originals = append(originals, u.add(t, s, nil).ID)
		}
		name, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy", "IO_PROXY_EXPERIMENTAL_APP_SYNC": "true"}, true, undone+":/undone")
		p := setUpPhone(t, proxy, u, photos)
		eventuallyRestarting(t, p, 3*time.Minute, "the app to find its photos in Immich", func() bool {
			return p.queryInt(sqlRemote) == len(photos) && p.queryInt(sqlNotBackedUp) == 0
		})

		logDir := sharedDir(t)
		code, out, log := runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
		if code != 0 {
			t.Fatalf("library run exited %d:\n%s", code, out)
		}
		for _, id := range originals {
			if replacedBy(log, id) == "" {
				t.Fatalf("library mode did not convert %s:\n%s", id, out)
			}
		}
		// The conversions bring hidden entries, next to the originals that
		// are still in the trash.
		eventuallyRestarting(t, p, 3*time.Minute, "the hidden entries of the conversions to reach the app", func() bool {
			return p.queryInt(sqlHidden+" and deleted_at is null") == len(photos)
		})

		// The trash is emptied: the originals' own entries go, the hidden
		// ones stay, and the phone has nothing to upload.
		must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": originals, "force": true}, nil)
		eventuallyRestarting(t, p, 3*time.Minute, "the app to forget the deleted originals", func() bool {
			return p.queryInt(sqlRemote) == len(photos) && p.queryInt("select count(*) from remote_asset_entity where library_id is null and deleted_at is not null") == 0
		})
		for i := 0; i < 2; i++ {
			p.restart()
			time.Sleep(15 * time.Second)
		}
		if n := p.queryInt(sqlNotBackedUp); n != 0 {
			t.Fatalf("the app has %d photo(s) to back up after the originals left the trash, want 0", n)
		}
		if n := received(t, name); n != 0 {
			t.Fatalf("the app uploaded %d photo(s) again after library mode converted them", n)
		}
		if live := u.live(t); len(live) != len(photos) {
			t.Fatalf("Immich has %d assets, want the %d conversions", len(live), len(photos))
		}
	})

	// What the app does with the proxy's error answers: 415 for a type no
	// task handles, 502 for a conversion that failed, 503 when the proxy has
	// no room. Nothing may be lost and the app must not hammer the proxy;
	// once the cause is gone, everything is backed up.
	errorCase := func(t *testing.T, env map[string]string, wantStored int, mounts ...string) {
		u := newUser(t)
		undone := sharedDir(t)
		port := freePort(t)
		samples := []string{"photo.jpg", "photo.png", "clip.mp4"}
		env["IO_MODE"] = "proxy"
		broken, proxy := startOptimizerOn(t, env, true, port, append(mounts, undone+":/undone")...)
		p := setUpPhone(t, proxy, u, samples)

		eventually(t, 3*time.Minute, "the app to try every file", func() bool { return received(t, broken) >= len(samples) })
		// What works is stored although its neighbours failed.
		eventually(t, 2*time.Minute, "the files that can be stored to be stored", func() bool { return len(u.live(t)) == wantStored })
		// Left alone, and looking again twice, the app does not flood the proxy.
		time.Sleep(30 * time.Second)
		idle := received(t, broken)
		for i := 0; i < 2; i++ {
			p.restart()
			time.Sleep(20 * time.Second)
		}
		after := received(t, broken)
		logs, _ := exec.Command("docker", "logs", broken).CombinedOutput()
		t.Logf("uploads received by the failing proxy: %d after the first round, %d after two more starts of the app", idle, after)
		if after > 4*len(samples) {
			t.Errorf("the app sent %d uploads for %d files: it keeps retrying without pause\n%s", after, len(samples), logs)
		}
		if n := len(u.live(t)); n != wantStored {
			t.Errorf("Immich has %d assets while the proxy fails the rest, want %d", n, wantStored)
		}

		// The cause is fixed: the same address, a proxy that works.
		if out, err := exec.Command("docker", "rm", "-f", broken).CombinedOutput(); err != nil {
			t.Fatalf("docker rm: %v\n%s", err, out)
		}
		startOptimizerOn(t, map[string]string{"IO_MODE": "proxy", "IO_PROXY_EXPERIMENTAL_APP_SYNC": "true"}, true, port, sharedDir(t)+":/undone")
		eventuallyRestarting(t, p, 5*time.Minute, "the app to back everything up once the proxy works", func() bool {
			return len(u.live(t)) == len(samples) && p.queryInt(sqlNotBackedUp) == 0
		})
	}

	t.Run("the app copes with refused and failed conversions", func(t *testing.T) {
		cfg := sharedDir(t)
		// JPEG is converted, PNG fails (502), and nothing handles video (415).
		tasks := "tasks:\n" +
			"  - name: jpeg-xl\n    command: cjxl --lossless_jpeg=1 -d 0 \"{{.src_folder}}/{{.name}}.{{.extension}}\" \"{{.dst_folder}}/{{.name}}.jxl\"\n    extensions: [jpg]\n" +
			"  - name: broken\n    command: exit 3\n    extensions: [png]\n"
		if err := os.WriteFile(filepath.Join(cfg, "tasks.yaml"), []byte(tasks), 0o644); err != nil {
			t.Fatal(err)
		}
		errorCase(t, map[string]string{"IO_TASKS_FILE": "/custom/tasks.yaml"}, 1, cfg+":/custom:ro")
	})

	t.Run("the app copes with a proxy that has no room", func(t *testing.T) {
		// A temp folder too small for any upload: every one is answered 503.
		errorCase(t, map[string]string{}, 0, "--tmpfs=/tmp:size=16k")
	})
}

// eventuallyRestarting is eventually for things the app only does when it
// syncs or looks for things to back up: it starts the app again now and then.
func eventuallyRestarting(t *testing.T, p phone, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for i := 0; i < 20; i++ {
			if check() {
				return
			}
			time.Sleep(time.Second)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		p.restart()
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}
