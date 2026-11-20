//go:build e2e

package e2e

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
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

// typeInto fills the i-th of n text fields. Typing is retried, because the
// app sometimes drops what is typed while the keyboard is still opening.
func (p phone) typeInto(i, n int, text string) {
	p.t.Helper()
	for try := 0; try < 5; try++ {
		f := p.fields(n)[i]
		if f.Text != "" {
			return
		}
		x1, y1, x2, y2 := f.box()
		p.tapAt((x1+x2)/2, (y1+y2)/2)
		time.Sleep(2 * time.Second)
		p.shell("input text '" + text + "'")
		time.Sleep(time.Second)
	}
	if p.fields(n)[i].Text == "" {
		p.t.Fatalf("could not type into text field %d", i)
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
	t.Cleanup(wipe)

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

// The reason the experimental app sync exists: the app decides what to back
// up by comparing the phone's files with the checksums the server reports,
// and a converted photo has a different one. This runs the same backup with
// the setting off and on, in the real app.
func TestAndroidAppBackup(t *testing.T) {
	if androidSerial == "" || adbPath == "" {
		t.Skip("needs an Android emulator: run with E2E_ANDROID=1 scripts/e2e-test.sh")
	}
	samples := []string{"photo.jpg", "plain.jpg", "photo.png"}

	t.Run("without app sync the phone uploads converted photos again", func(t *testing.T) {
		u := newUser(t)
		undone := sharedDir(t)
		name, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")
		p := setUpPhone(t, proxy, u, samples)

		eventually(t, 3*time.Minute, "the app to upload the photos", func() bool { return uploads(t, undone) >= len(samples) })
		eventually(t, 2*time.Minute, "the app to learn about the converted assets", func() bool { return p.queryInt(sqlRemote) == len(samples) })
		if n := p.queryInt(sqlNotBackedUp); n != len(samples) {
			t.Fatalf("the app has %d photo(s) left to back up, want all %d: the converted checksums should not match", n, len(samples))
		}
		// So when it looks again, it sends them all again.
		p.restart()
		eventually(t, 2*time.Minute, "the app to upload the photos a second time", func() bool { return received(t, name) >= 2*len(samples) })
	})

	t.Run("with app sync the phone sees them as backed up and uploads once", func(t *testing.T) {
		u := newUser(t)
		undone := sharedDir(t)
		name, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy", "IO_PROXY_EXPERIMENTAL_APP_SYNC": "true"}, true, undone+":/undone")
		p := setUpPhone(t, proxy, u, samples)

		eventually(t, 3*time.Minute, "the app to upload the photos", func() bool { return uploads(t, undone) >= len(samples) })
		// Right after an upload the app hears about the new asset over the
		// websocket, which the proxy does not touch. The hidden entries come
		// with its next sync, which it runs when it starts and before every
		// backup round.
		eventually(t, 2*time.Minute, "the app to learn about the converted assets", func() bool { return p.queryInt(sqlRemote) == len(samples) })
		p.restart()
		eventually(t, 2*time.Minute, "the hidden entries to reach the app", func() bool { return p.queryInt(sqlHidden) == len(samples) })
		eventually(t, time.Minute, "the app to count every photo as backed up", func() bool { return p.queryInt(sqlNotBackedUp) == 0 })

		// The hidden entries carry the checksums of the phone's files.
		for _, s := range samples {
			sum := sha1Base64(t, filepath.Join(mediaDir, s))
			if n := p.queryInt("select count(*) from remote_asset_entity where visibility = 1 and library_id = '" + hiddenLibrary + "' and checksum = '" + sum + "'"); n != 1 {
				t.Errorf("%s: %d hidden entries with the original's checksum, want 1", s, n)
			}
		}

		// Starting the app again makes it look for things to back up. Nothing
		// may be uploaded a second time.
		for i := 0; i < 2; i++ {
			p.restart()
			time.Sleep(15 * time.Second)
		}
		if n := received(t, name); n != len(samples) {
			t.Fatalf("the proxy received %d uploads, want %d: the app uploaded again", n, len(samples))
		}
		if n := p.queryInt(sqlNotBackedUp); n != 0 {
			t.Fatalf("after restarting, the app has %d photo(s) left to back up, want 0", n)
		}
		if live := u.live(t); len(live) != len(samples) {
			t.Fatalf("Immich has %d assets, want %d", len(live), len(samples))
		}

		// What the user sees on the backup page.
		p.tap("Button", "Backup")
		got := p.wait("", "Remainder").label()
		if !strings.Contains(got, "/ 0 /") {
			t.Fatalf("the app's backup page says %q, want a remainder of 0", got)
		}
	})
}
