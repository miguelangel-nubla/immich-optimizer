//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests drive the real Immich iOS app in Xcode's simulator. They run
// with E2E_IOS=1 scripts/e2e-test.sh on a Mac, which brings the simulated
// iPhone up with scripts/ios-simulator.sh and sets the variables below.
var (
	iosScript    = os.Getenv("E2E_IOS_SIMULATOR")
	iosUDID      = os.Getenv("E2E_IOS_UDID")
	iosDevices   = os.Getenv("E2E_IOS_DEVICES")
	iosApp       = os.Getenv("E2E_IOS_APP")
	iosBundle    = os.Getenv("E2E_IOS_BUNDLE")
	idbPath      = os.Getenv("E2E_IDB")
	idbCompanion = os.Getenv("E2E_IDB_COMPANION")
)

// iphone is the simulated phone: simctl manages it, idb reads its screen and
// taps on it.
type iphone struct {
	t  *testing.T
	db *string // the app's own database, once the app is installed
}

func (p iphone) simTry(args ...string) (string, error) {
	out, err := exec.Command("xcrun", append([]string{"simctl", "--set", iosDevices}, args...)...).CombinedOutput()
	return string(out), err
}

func (p iphone) sim(args ...string) string {
	p.t.Helper()
	out, err := p.simTry(args...)
	if err != nil {
		p.t.Fatalf("simctl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// idb is tried a few times: right after the phone has booted, its helper
// refuses now and then.
func (p iphone) idb(args ...string) (out string, err error) {
	for try := 0; try < 5; try++ {
		var b []byte
		b, err = exec.Command(idbPath, append([]string{"--companion", idbCompanion}, args...)...).Output()
		out = string(b)
		if err == nil {
			return out, nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			out += string(ee.Stderr)
		}
		time.Sleep(3 * time.Second)
	}
	return out, err
}

// element is one element of the screen, as iOS's accessibility tree has it.
type element struct {
	Type  string `json:"type"`
	Label string `json:"AXLabel"`
	Value string `json:"AXValue"`
	Frame struct {
		X, Y, Width, Height float64
	} `json:"frame"`
}

func (e element) text() string { return strings.ReplaceAll(e.Label, "\n", " / ") }

func (e element) centre() (int, int) {
	return int(e.Frame.X + e.Frame.Width/2), int(e.Frame.Y + e.Frame.Height/2)
}

// screen returns the elements on screen, or nil when it cannot be read.
func (p iphone) screen() []element {
	out, err := p.idb("ui", "describe-all")
	if err != nil {
		return nil
	}
	var els []element
	if json.Unmarshal([]byte(out), &els) != nil {
		return nil
	}
	return els
}

// findEl returns the first element of the given type ("" for any) whose label
// starts with prefix.
func findEl(els []element, typ, prefix string) (element, bool) {
	for _, e := range els {
		if (typ == "" || e.Type == typ) && strings.HasPrefix(e.text(), prefix) {
			return e, true
		}
	}
	return element{}, false
}

// systemPrompts are the buttons that get iOS's own questions out of the way:
// notifications, the photo library, saving the password.
var systemPrompts = []string{"Allow Full Access", "Allow", "Not Now"}

// answerPrompt taps away a question from iOS if one is showing.
func (p iphone) answerPrompt(els []element) bool {
	for _, label := range systemPrompts {
		for _, e := range els {
			if e.Type == "Button" && e.text() == label {
				p.tapAt(e.centre())
				time.Sleep(2 * time.Second)
				return true
			}
		}
	}
	return false
}

// wait returns the element once it is on screen, answering iOS's questions
// on the way.
func (p iphone) wait(typ, prefix string) element {
	p.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for blank := 0; ; {
		els := p.screen()
		if e, ok := findEl(els, typ, prefix); ok {
			return e
		}
		// "Save Password?" covers the app after the login and is the one
		// question that cannot be read: the screen shows up empty. Its
		// "Not Now" is at the bottom.
		if len(els) == 1 {
			if blank++; blank%5 == 0 {
				p.tapAt(passwordSheetNotNow())
			}
		}
		if !p.answerPrompt(els) && time.Now().After(deadline) {
			p.t.Fatalf("%s %q did not appear in the app; on screen:\n%s", typ, prefix, describe(els))
		}
		time.Sleep(time.Second)
	}
}

// passwordSheetNotNow is where the "Not Now" of iOS's password sheet is on
// the simulated iPhone 16 (393 by 852 points).
func passwordSheetNotNow() (int, int) { return 197, 789 }

func describe(els []element) string {
	var seen []string
	for _, e := range els {
		x, y := e.centre()
		seen = append(seen, fmt.Sprintf("%d,%d %s %q %q", x, y, e.Type, e.text(), e.Value))
	}
	return strings.Join(seen, "\n")
}

func (p iphone) tapAt(x, y int) {
	p.t.Helper()
	if out, err := p.idb("ui", "tap", strconv.Itoa(x), strconv.Itoa(y)); err != nil {
		p.t.Fatalf("tap: %v\n%s", err, out)
	}
}

// tap waits for the element and taps its centre.
func (p iphone) tap(typ, prefix string) {
	p.t.Helper()
	p.tapAt(p.wait(typ, prefix).centre())
}

// typeInto fills the text field with the given label and checks what ended
// up in it.
func (p iphone) typeInto(typ, prefix, text string) {
	p.t.Helper()
	// A password field shows a dot for every character.
	typed := func(got string) bool {
		return got == text || len([]rune(got)) == len([]rune(text)) && strings.Trim(got, "•*") == ""
	}
	for try := 0; try < 5; try++ {
		f := p.wait(typ, prefix)
		if typed(f.Value) {
			return
		}
		p.tapAt(f.centre())
		time.Sleep(time.Second)
		if n := len([]rune(f.Value)); n > 0 {
			keys := make([]string, n+2)
			for i := range keys {
				keys[i] = "42" // backspace
			}
			_, _ = p.idb(append([]string{"ui", "key-sequence"}, keys...)...)
		}
		if out, err := p.idb("ui", "text", text); err != nil {
			p.t.Fatalf("typing: %v\n%s", err, out)
		}
		time.Sleep(time.Second)
	}
	p.t.Fatalf("could not type into %q: it holds %q", prefix, p.wait(typ, prefix).Value)
}

func (p iphone) launch() {
	p.t.Helper()
	p.sim("launch", iosUDID, iosBundle)
}

func (p iphone) stop() { _, _ = p.simTry("terminate", iosUDID, iosBundle) }

func (p iphone) restart() {
	p.t.Helper()
	p.stop()
	p.launch()
	p.wait("Button", "Backup")
}

// queryInt runs a counting query on the app's own database; it returns -1
// while the database is busy or not there yet.
func (p iphone) queryInt(sql string) int {
	out, err := exec.Command("sqlite3", "-readonly", *p.db, sql).Output()
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return n
}

func (p iphone) query(sql ...string) string {
	out, _ := exec.Command("sqlite3", append([]string{"-readonly", *p.db}, sql...)...).CombinedOutput()
	return string(out)
}

// addMedia puts files into the phone's photo library. Given a photo and a
// video that belong together, iOS makes one Live Photo of them.
func (p iphone) addMedia(files ...string) {
	p.t.Helper()
	// The simulator cannot read an external volume, where the checkout may be.
	dir := p.t.TempDir()
	args := []string{"addmedia", iosUDID}
	for _, f := range files {
		in, err := os.Open(f)
		if err != nil {
			p.t.Fatal(err)
		}
		dst := filepath.Join(dir, filepath.Base(f))
		out, err := os.Create(dst)
		if err != nil {
			p.t.Fatal(err)
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			p.t.Fatal(err)
		}
		args = append(args, dst)
	}
	p.sim(args...)
}

// setUpIPhone gives the test a phone as it comes out of the box plus the
// given photos, with the app logged in to the proxy as u and backup on for
// every photo.
func setUpIPhone(t *testing.T, proxyURL string, u *user, samples []string) iphone {
	t.Helper()
	p := iphone{t: t, db: new(string)}
	// Wiping the phone is the only way to empty its photo library.
	if out, err := exec.Command(iosScript, "reset").CombinedOutput(); err != nil {
		t.Fatalf("resetting the simulated phone: %v\n%s", err, out)
	}
	*p.db = filepath.Join(strings.TrimSpace(p.sim("get_app_container", iosUDID, iosBundle, "data")), "Documents", "immich.sqlite")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("the app's database:\n%s", p.query(
				"select 'local', name, checksum from local_asset_entity",
				"select 'remote', id, name, checksum, visibility, library_id, deleted_at from remote_asset_entity"))
			t.Logf("on screen:\n%s", describe(p.screen()))
		}
		p.stop()
	})
	for _, s := range samples {
		p.addMedia(filepath.Join(mediaDir, s))
	}

	// The simulator shares the Mac's network: the proxy's address works as is.
	p.launch()
	p.typeInto("TextField", "Server Endpoint URL", proxyURL)
	p.tap("Button", "Next")
	p.typeInto("TextField", "Email", u.email)
	// The password field does not say what it holds: typed once, unchecked.
	p.tap("TextField", "Password")
	time.Sleep(time.Second)
	if out, err := p.idb("ui", "text", password); err != nil {
		t.Fatalf("typing: %v\n%s", err, out)
	}
	p.tap("Button", "Login")
	p.wait("Button", "Backup")

	// Backup on, for everything on the phone.
	p.tap("Button", "Backup")
	p.tap("Button", "Select")
	p.tap("Button", "Recents")
	time.Sleep(time.Second)
	p.tapAt(p.wait("Button", "").centre()) // the first button is "back"
	p.wait("StaticText", "Remainder")
	p.setBackup(true)
	return p
}

// setBackup flips the "Enable Backup" switch on the app's backup page.
func (p iphone) setBackup(on bool) {
	p.t.Helper()
	want := "0"
	if on {
		want = "1"
	}
	for try := 0; try < 5; try++ {
		sw := p.wait("CheckBox", "Enable Backup")
		if sw.Value == want {
			return
		}
		// The switch itself is at the right end of the row.
		_, y := sw.centre()
		p.tapAt(int(sw.Frame.X+sw.Frame.Width)-50, y)
		time.Sleep(3 * time.Second)
	}
	p.t.Fatalf("could not switch backup in the app")
}

// editAPhoto changes one of the phone's photos in the Photos app, the way a
// person would: open it, rotate it, save.
func (p iphone) editAPhoto() {
	p.t.Helper()
	p.sim("launch", iosUDID, "com.apple.mobileslideshow")
	// Until a photo is open: its "Edit" button shows. On a fresh phone the
	// Photos app first shows "What's New", sometimes only after its grid.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		els := p.screen()
		if e, ok := findEl(els, "Button", "Edit"); ok {
			p.tapAt(e.centre())
			break
		}
		switch e, ok := findEl(els, "Button", "Continue"); {
		case ok:
			p.tapAt(e.centre())
		case p.answerPrompt(els):
		default:
			if _, grid := findEl(els, "Button", "Select"); grid {
				// The grid's pictures are not named on screen. The newest
				// is at the end of the bottom row, at this place when the
				// library is small. The simulator's own photos cannot be
				// edited, so the test edits one it added.
				p.tapAt(354, 532)
			} else if time.Now().After(deadline) {
				p.t.Fatalf("could not open a photo in the Photos app; on screen:\n%s", describe(els))
			}
		}
		time.Sleep(2 * time.Second)
	}
	p.tap("Button", "Crop and Rotate")
	p.tap("Button", "Rotate")
	time.Sleep(2 * time.Second)
	// "Done" is drawn off the simulator's screen; Return does the same.
	if out, err := p.idb("ui", "key", "40"); err != nil {
		p.t.Fatalf("saving the edit: %v\n%s", err, out)
	}
	p.wait("Button", "Back")
	time.Sleep(3 * time.Second)
}

// triggerBackground makes iOS run the app's background backup now. On a real
// phone iOS picks the moment; in the simulator only the debugger can ask for
// it, as Immich's own source documents (BackgroundWorker.swift). It reports
// false when this Mac does not let the debugger attach without asking.
func (p iphone) triggerBackground() bool {
	p.t.Helper()
	if out, _ := exec.Command("DevToolsSecurity", "-status").CombinedOutput(); !strings.Contains(string(out), "enabled") {
		return false
	}
	var pid string
	for _, l := range strings.Split(p.sim("spawn", iosUDID, "launchctl", "list"), "\n") {
		if strings.Contains(l, "UIKitApplication:"+iosBundle) {
			pid = strings.Fields(l)[0]
		}
	}
	if _, err := strconv.Atoi(pid); err != nil {
		p.t.Fatalf("the app is not running (%q)", pid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xcrun", "lldb", "--batch", "-p", pid,
		"-o", `e -l objc -- (void)[[BGTaskScheduler sharedScheduler] _simulateLaunchForTaskWithIdentifier:@"app.alextran.immich.background.processingUpload"]`,
		"-o", "detach").CombinedOutput()
	if err != nil {
		p.t.Fatalf("asking for the background task: %v\n%s", err, out)
	}
	return true
}

// The same as the Android test, on the iOS app, plus what only an iPhone
// has: the iCloud record the app keeps for every asset, Live Photos, photos
// edited in the Photos app and iOS's background task.
func TestIOSAppBackup(t *testing.T) {
	if iosUDID == "" || idbPath == "" {
		t.Skip("needs the iOS simulator: run with E2E_IOS=1 scripts/e2e-test.sh on a Mac")
	}
	u := newUser(t)
	undone := sharedDir(t)
	name, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy"}, true, undone+":/undone")
	samples := []string{"photo.jpg", "plain.jpg", "photo.png"}
	p := setUpIPhone(t, proxy, u, samples)

	checksum := func(sample string) string { return sha1Base64(t, filepath.Join(mediaDir, sample)) }
	hidden := func(sample, more string) int {
		return p.queryInt("select count(*) from remote_asset_entity where visibility = 1 and library_id = '" + hiddenLibrary +
			"' and checksum = '" + checksum(sample) + "'" + more)
	}
	// freeable is the app's own test for "Free up space": a photo on the
	// phone whose checksum the server has, outside the trash.
	freeable := func(sample string) int {
		return p.queryInt("select count(*) from local_asset_entity l join remote_asset_entity r on r.checksum = l.checksum" +
			" where r.deleted_at is null and l.checksum = '" + checksum(sample) + "'")
	}
	const sqlLocal = "select count(*) from local_asset_entity"
	// settled waits until everything on the phone counts as backed up, and
	// returns how many uploads that took in all.
	settled := func(uploads int, what string) {
		t.Helper()
		eventuallyRestarting(t, p, 4*time.Minute, what+" to be uploaded", func() bool { return received(t, name) >= uploads })
		eventuallyRestarting(t, p, 3*time.Minute, what+" to count as backed up", func() bool { return p.queryInt(sqlNotBackedUp) == 0 })
		// Starting the app again makes it sync and look for things to back
		// up. Nothing may be uploaded a second time.
		for i := 0; i < 2; i++ {
			p.restart()
			time.Sleep(15 * time.Second)
		}
		if n := received(t, name); n != uploads {
			t.Fatalf("%s: the proxy received %d uploads, want %d: the app uploaded again", what, n, uploads)
		}
		if n := p.queryInt(sqlNotBackedUp); n != 0 {
			t.Fatalf("%s: after restarting, the app has %d file(s) left to back up, want 0", what, n)
		}
	}

	// A new simulated phone comes with a few photos of its own; they are
	// backed up with the samples.
	eventually(t, time.Minute, "the app to list the phone's photos", func() bool { return p.queryInt(sqlLocal) >= len(samples) })
	uploads := p.queryInt(sqlLocal)
	eventually(t, 4*time.Minute, "the app to upload everything", func() bool { return received(t, name) >= uploads })
	// Without the app being restarted: the hidden entries come over the
	// websocket, right behind Immich's own message about each upload.
	eventually(t, 3*time.Minute, "the app to count everything as backed up", func() bool { return p.queryInt(sqlNotBackedUp) == 0 })
	settled(uploads, "the first backup")
	for _, s := range samples {
		if n := hidden(s, ""); n != 1 {
			t.Errorf("%s: %d hidden entries with the original's checksum, want 1", s, n)
		}
		if n := freeable(s); n != 1 {
			t.Errorf("%s: the app would not offer to free it up (%d)", s, n)
		}
	}
	if live := u.live(t); len(live) != uploads {
		t.Fatalf("Immich has %d assets, want %d", len(live), uploads)
	}

	// The iOS app keeps an iCloud record for every asset and sends the
	// server those it has none for, each time it starts. The hidden entries
	// get theirs from the proxy, so the app has nothing to send for them.
	eventuallyRestarting(t, p, 3*time.Minute, "the hidden entries to get their iCloud records", func() bool {
		n := p.queryInt(sqlHidden)
		return n >= len(samples) && p.queryInt("select count(*) from remote_asset_cloud_id_entity c join remote_asset_entity r on r.id = c.asset_id"+
			" where r.library_id = '"+hiddenLibrary+"' and c.cloud_id is not null") == n
	})

	// What the user sees on the backup page.
	p.tap("Button", "Backup")
	if got := p.wait("StaticText", "Remainder").text(); !strings.Contains(got, "/ 0 /") {
		t.Fatalf("the app's backup page says %q, want a remainder of 0", got)
	}

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
	if n := received(t, name); n != uploads {
		t.Fatalf("the proxy received %d uploads, want %d: the app uploaded a trashed photo again", n, uploads)
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
	uploads++
	settled(uploads, "the photo deleted on the server")
	if hidden("photo.jpg", " and deleted_at is null") != 1 {
		t.Fatalf("the photo uploaded again has no hidden entry")
	}
	if id2 := u.assetOf(t, "photo.jpg"); id2 == id {
		t.Fatalf("the deleted asset %s is still there", id)
	}

	// A Live Photo is a photo and a short video that belong together. The
	// app uploads both and tells Immich they are one; the photo is converted
	// and must stay paired.
	p.addMedia(filepath.Join(mediaDir, "live.jpg"), filepath.Join(mediaDir, "live.mov"))
	uploads += 2
	settled(uploads, "the Live Photo")
	if n := p.queryInt("select count(*) from local_asset_entity where checksum = '" + checksum("live.jpg") + "'"); n != 1 {
		t.Fatalf("the phone did not take the pair as one Live Photo (%d)", n)
	}
	still := u.asset(t, u.assetOf(t, "live.jpg"))
	if still.LivePhotoVideoID == nil {
		t.Fatalf("the converted Live Photo lost its video")
	}
	if still.OriginalMimeType != "image/jxl" {
		t.Errorf("the Live Photo's still is %s, want it converted", still.OriginalMimeType)
	}
	if n := hidden("live.jpg", ""); n != 1 {
		t.Errorf("the Live Photo has %d hidden entries, want 1", n)
	}

	// A photo edited on the phone: iOS keeps the original, and the app
	// backs up the edited picture as a new file.
	p.addMedia(filepath.Join(mediaDir, "photo.heic"))
	uploads++
	settled(uploads, "the photo to edit")
	p.stop()
	p.editAPhoto()
	uploads++
	settled(uploads, "the edited photo")
	if n := p.queryInt("select count(*) from local_asset_entity where adjustment_time is not null"); n != 1 {
		t.Fatalf("%d photos on the phone count as edited, want 1", n)
	}

	// iOS's background task: what backs photos up while the app is not open.
	t.Run("in the background", func(t *testing.T) {
		p.t = t
		// Another app in front: Immich keeps running behind it.
		p.sim("launch", iosUDID, "com.apple.mobileslideshow")
		time.Sleep(5 * time.Second)
		p.addMedia(filepath.Join(mediaDir, "motion.jpg"))
		time.Sleep(10 * time.Second)
		if !p.triggerBackground() {
			t.Skip("the debugger may not attach on this Mac without asking: run `sudo DevToolsSecurity -enable` once")
		}
		eventually(t, 3*time.Minute, "the background task to upload the new photo", func() bool { return received(t, name) >= uploads+1 })
		p.launch()
		p.wait("Button", "Backup")
		eventuallyRestarting(t, p, 3*time.Minute, "the photo uploaded in the background to count as backed up", func() bool {
			return p.queryInt(sqlNotBackedUp) == 0 && hidden("motion.jpg", "") == 1
		})
		if n := received(t, name); n != uploads+1 {
			t.Fatalf("the proxy received %d uploads, want %d", n, uploads+1)
		}
	})
}
