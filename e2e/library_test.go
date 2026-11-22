//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// richOriginal uploads photo.jpg and gives it everything library mode has to
// carry over: favorite, archive, description, rating, a tag, a named face, an
// album, an individual shared link and a place in a stack as its primary.
type rich struct {
	a        asset
	albumID  string
	tagID    string
	personID string
	linkID   string
	stackID  string
	other    asset // the second member of the stack
}

func richOriginal(t *testing.T, u *user) rich {
	t.Helper()
	var r rich
	r.a = u.add(t, "photo.jpg", map[string]string{"isFavorite": "true"})
	id := r.a.ID

	must(t, u.key, http.MethodPut, "/assets/"+id, map[string]any{"description": "holiday", "rating": 4, "visibility": "archive"}, nil)

	var tag, person, album, link, stack struct {
		ID string `json:"id"`
	}
	must(t, u.key, http.MethodPost, "/tags", map[string]any{"name": "trip"}, &tag)
	must(t, u.key, http.MethodPut, "/tags/assets", map[string]any{"assetIds": []string{id}, "tagIds": []string{tag.ID}}, nil)
	must(t, u.key, http.MethodPost, "/people", map[string]any{"name": "Alice"}, &person)
	if s := call(t, u.key, http.MethodPost, "/faces", map[string]any{"assetId": id, "personId": person.ID,
		"x": 100, "y": 80, "width": 120, "height": 120, "imageWidth": 640, "imageHeight": 480}, nil); s >= 300 {
		t.Fatalf("creating face: status %d", s)
	}
	must(t, u.key, http.MethodPost, "/albums", map[string]any{"albumName": "Album", "assetIds": []string{id}}, &album)
	must(t, u.key, http.MethodPost, "/shared-links", map[string]any{"type": "INDIVIDUAL", "assetIds": []string{id}}, &link)
	r.other = u.add(t, "photo.png", nil)
	must(t, u.key, http.MethodPost, "/stacks", map[string]any{"assetIds": []string{id, r.other.ID}}, &stack)

	r.albumID, r.tagID, r.personID, r.linkID, r.stackID = album.ID, tag.ID, person.ID, link.ID, stack.ID
	r.a = u.asset(t, id)
	if r.a.Visibility != "archive" || r.a.ExifInfo.Description != "holiday" || len(r.a.Tags) != 1 || len(r.a.People) != 1 || r.a.Stack == nil {
		t.Fatalf("test setup incomplete: %+v", r.a)
	}
	return r
}

func TestLibraryReplacesAssetAndCarriesEverythingOver(t *testing.T) {
	u := newUser(t)
	r := richOriginal(t, u)
	logDir := sharedDir(t)

	code, out, log := runLibrary(t, u, logDir, nil)
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	var newID string
	for _, e := range log {
		if e["event"] == "replaced" && e["asset"] == r.a.ID {
			newID, _ = e["new_asset"].(string)
		}
	}
	if newID == "" {
		t.Fatalf("no replacement logged for the photo:\n%s\nlog events: %v", out, events(log, ""))
	}

	old, n := u.asset(t, r.a.ID), u.asset(t, newID)
	if !old.IsTrashed {
		t.Error("the original is not in the trash")
	}
	if n.IsTrashed || n.OriginalFileName != "photo.jxl" || n.OriginalMimeType != "image/jxl" {
		t.Errorf("converted asset: %+v", n)
	}
	if n.ExifInfo.Size >= r.a.ExifInfo.Size {
		t.Errorf("converted file is not smaller: %d >= %d", n.ExifInfo.Size, r.a.ExifInfo.Size)
	}
	// Everything the original had.
	if !n.IsFavorite || n.Visibility != "archive" || n.ExifInfo.Description != "holiday" ||
		n.ExifInfo.Rating == nil || *n.ExifInfo.Rating != 4 {
		t.Errorf("edited fields not carried over: %+v", n)
	}
	if n.FileCreatedAt != r.a.FileCreatedAt || n.LocalDateTime != r.a.LocalDateTime {
		t.Errorf("dates differ: %s/%s vs %s/%s", n.FileCreatedAt, n.LocalDateTime, r.a.FileCreatedAt, r.a.LocalDateTime)
	}
	if n.ExifInfo.DateTimeOriginal == nil || r.a.ExifInfo.DateTimeOriginal == nil || *n.ExifInfo.DateTimeOriginal != *r.a.ExifInfo.DateTimeOriginal {
		t.Error("date taken differs")
	}
	if n.ExifInfo.Latitude == nil || n.ExifInfo.Width == nil || *n.ExifInfo.Width != 640 || n.ExifInfo.Model == nil || *n.ExifInfo.Model != "Fixture" {
		t.Errorf("metadata read from the file differs: %+v", n.ExifInfo)
	}
	if len(n.Tags) != 1 || n.Tags[0].ID != r.tagID {
		t.Errorf("tags = %+v", n.Tags)
	}
	if len(n.People) != 1 || n.People[0].ID != r.personID {
		t.Errorf("people = %+v", n.People)
	}
	if !contains(u.albumsOf(t, newID), r.albumID) {
		t.Error("not in the album")
	}
	if !contains(u.linkAssets(t, r.linkID), newID) {
		t.Error("not in the shared link")
	}
	if n.Stack == nil || n.Stack.ID != r.stackID || n.Stack.PrimaryAssetID != newID {
		t.Errorf("stack = %+v, want primary of %s", n.Stack, r.stackID)
	}

	// The marker records where it came from and is no longer pending.
	m := u.marker(t, newID)
	if m == nil || m["library.replaces"] != r.a.ID || m["original.sha1"] != hexChecksum(t, r.a.Checksum) || m["latest.source"] != "library" || m["library.state"] != nil {
		t.Errorf("marker = %v", m)
	}

	// The log tells the whole story, in order.
	ev := strings.Join(events(log, r.a.ID), " ")
	if !strings.Contains(ev, "converted uploaded verified replaced") {
		t.Errorf("log events for the photo: %s", ev)
	}

	// The PNG in the same stack was converted too (pixel-compared).
	if !contains(events(log, r.other.ID), "replaced") {
		t.Errorf("PNG not replaced: %v", events(log, r.other.ID))
	}

	// A second run finds nothing left to do.
	code, out, log2 := runLibrary(t, u, logDir, nil)
	if code != 0 {
		t.Fatalf("second run exited %d:\n%s", code, out)
	}
	for _, e := range log2[len(log):] {
		if e["event"] == "replaced" || e["event"] == "uploaded" || e["event"] == "failed" {
			t.Errorf("second run did something: %v", e)
		}
	}

	// Second chance: the original can be restored from Immich's trash.
	must(t, u.key, http.MethodPost, "/trash/restore/assets", map[string]any{"ids": []string{r.a.ID}}, nil)
	if back := u.asset(t, r.a.ID); back.IsTrashed || back.Checksum != r.a.Checksum {
		t.Errorf("restored original: %+v", back)
	}
	// ...and a run after that neither converts it again nor harms either copy.
	code, out, _ = runLibrary(t, u, logDir, nil)
	if code != 0 {
		t.Fatalf("run after restore exited %d:\n%s", code, out)
	}
	if u.asset(t, r.a.ID).IsTrashed || u.asset(t, newID).IsTrashed {
		t.Error("run after restore trashed something")
	}
}

func TestLibraryLeavesOtherAssetsAlone(t *testing.T) {
	u := newUser(t)
	heic := u.add(t, "photo.heic", nil) // passed through by the lossless profile
	clip := u.add(t, "clip.mp4", nil)   // no lossless video task

	// A photo with a comment from another user in a shared album.
	other := newUser(t)
	commented := u.add(t, "live.jpg", nil)
	var album struct {
		ID string `json:"id"`
	}
	must(t, u.key, http.MethodPost, "/albums", map[string]any{"albumName": "Shared", "assetIds": []string{commented.ID},
		"albumUsers": []map[string]string{{"userId": other.id, "role": "editor"}}}, &album)
	must(t, other.key, http.MethodPost, "/activities", map[string]any{"albumId": album.ID, "assetId": commented.ID, "type": "comment", "comment": "nice"}, nil)

	before := len(u.live(t))
	logDir := sharedDir(t)
	code, out, log := runLibrary(t, u, logDir, nil)
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	for _, a := range []asset{heic, clip, commented} {
		now := u.asset(t, a.ID)
		if now.IsTrashed || now.Checksum != a.Checksum {
			t.Errorf("%s was changed", a.OriginalFileName)
		}
		if contains(events(log, a.ID), "uploaded") {
			t.Errorf("%s: something was uploaded: %v", a.OriginalFileName, events(log, a.ID))
		}
	}
	if n := len(u.live(t)); n != before {
		t.Errorf("%d live assets, want %d", n, before)
	}
	// The other user's library is untouched and unseen.
	if n := len(other.live(t)); n != 0 {
		t.Errorf("other user has %d assets", n)
	}
}

func TestLibraryRejectsInvalidOutputAndAcceptsLossy(t *testing.T) {
	u := newUser(t)
	a := u.add(t, "photo.jpg", nil)

	// A task that "succeeds" but produces a single flat colour.
	cfg := sharedDir(t)
	// Each variant gets its own file: rewriting one mounted file can show
	// the container a stale copy.
	n := 0
	env := map[string]string{}
	write := func(command string) {
		n++
		name := fmt.Sprintf("tasks-%d.yaml", n)
		// A block scalar, so the command needs no YAML quoting.
		tasks := "tasks:\n  - name: custom\n    command: >-\n      " + command + "\n    extensions: [jpg]\n"
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(tasks), 0o644); err != nil {
			t.Fatal(err)
		}
		env["IO_TASKS_FILE"] = "/custom/" + name
	}
	write(`magick -size 640x480 xc:gray "{{.dst_folder}}/{{.name}}.png"`)
	logDir := sharedDir(t)
	code, out, log := runLibrary(t, u, logDir, env, cfg+":/custom:ro")
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	ev := events(log, a.ID)
	if !contains(ev, "failed") || contains(ev, "uploaded") || contains(ev, "replaced") {
		t.Errorf("events = %v", ev)
	}
	if !strings.Contains(out, "single flat colour") {
		t.Errorf("the reason is not in the output:\n%s", out)
	}
	now := u.asset(t, a.ID)
	if now.IsTrashed || now.Checksum != a.Checksum || len(u.live(t)) != 1 || u.marker(t, a.ID) != nil {
		t.Error("an invalid conversion changed the library")
	}

	// A failed asset is tried again once its failed tag is removed.
	if !contains(u.tags(t, a.ID), tagFailed) {
		t.Errorf("not tagged failed: %v", u.tags(t, a.ID))
	}
	u.untag(t, tagFailed, a.ID)

	// A task that writes garbage is rejected too.
	write(`head -c 2000 /dev/urandom > "{{.dst_folder}}/{{.name}}.jxl"`)
	code, out, log2 := runLibrary(t, u, logDir, env, cfg+":/custom:ro")
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	if ev := events(log2[len(log):], a.ID); !contains(ev, "failed") || contains(ev, "uploaded") {
		t.Errorf("events = %v", ev)
	}
	if u.asset(t, a.ID).IsTrashed {
		t.Error("garbage output replaced the original")
	}

	u.untag(t, tagFailed, a.ID)

	// A lossy task that also shrinks the picture is a valid conversion.
	write(`magick "{{.src_folder}}/{{.name}}.{{.extension}}" -resize 50% ppm:- | cjxl - "{{.dst_folder}}/{{.name}}.jxl" -q 75 && exiftool -q -m -overwrite_original -tagsfromfile "{{.src_folder}}/{{.name}}.{{.extension}}" -all:all "{{.dst_folder}}/{{.name}}.jxl"`)
	code, out, log3 := runLibrary(t, u, logDir, env, cfg+":/custom:ro")
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	if ev := events(log3[len(log2):], a.ID); !contains(ev, "replaced") {
		t.Fatalf("lossy resize was not accepted: %v\n%s", ev, out)
	}
	if !u.asset(t, a.ID).IsTrashed {
		t.Error("original not trashed after a valid lossy conversion")
	}
	live := u.live(t)
	if len(live) != 1 || live[0].ExifInfo.Width == nil || *live[0].ExifInfo.Width != 320 {
		t.Errorf("live assets = %+v", live)
	}
}

// convertSample produces the exact bytes the lossless profile makes from a
// sample, using the image's own cjxl.
func convertSample(t *testing.T, sample string) string {
	t.Helper()
	dir := sharedDir(t)
	out, err := exec.Command("docker", "run", "--rm", "-v", mediaDir+":/m:ro", "-v", dir+":/o", "--entrypoint", "cjxl", image,
		"--lossless_jpeg=1", "-d", "0", "/m/"+sample, "/o/converted.jxl").CombinedOutput()
	if err != nil {
		t.Fatalf("cjxl: %v\n%s", err, out)
	}
	return filepath.Join(dir, "converted.jxl")
}

func TestLibraryFinishesAnInterruptedReplacement(t *testing.T) {
	jxl := convertSample(t, "photo.jpg")

	t.Run("interrupted before the original was trashed", func(t *testing.T) {
		u := newUser(t)
		a := u.add(t, "photo.jpg", nil)
		// What a crashed run leaves behind: the converted copy, marked pending.
		status, out := u.upload(t, immichURL, jxl, "photo.jxl", map[string]string{
			"metadata": `[{"key":"immich-optimizer","value":{"v":1,"original":{"sha1":"` + hexChecksum(t, a.Checksum) + `","size":1},"conversion":{"source":"library","task":{"name":"jpeg-xl"}},"library":{"replaces":"` + a.ID + `","state":"pending"}}}]`})
		if status != http.StatusCreated {
			t.Fatalf("seeding the pending copy: %d %s", status, out["_body"])
		}
		pending := u.waitReady(t, out["id"])

		code, logs, log := runLibrary(t, u, sharedDir(t), nil)
		if code != 0 {
			t.Fatalf("library run exited %d:\n%s", code, logs)
		}
		// Depending on which of the two the run reaches first, the pending
		// copy is completed, or removed and made again. Either way exactly
		// one finished converted asset remains and the original is trashed.
		_ = pending
		if !u.asset(t, a.ID).IsTrashed {
			t.Errorf("original not trashed; events: %v", events(log, ""))
		}
		live := u.live(t)
		if len(live) != 1 {
			t.Fatalf("%d live assets, want 1", len(live))
		}
		if m := u.marker(t, live[0].ID); m == nil || m["library.state"] != nil || m["library.replaces"] != a.ID {
			t.Errorf("marker = %v", m)
		}
		if live[0].OriginalFileName != "photo.jxl" {
			t.Errorf("live asset is %s", live[0].OriginalFileName)
		}
	})

	t.Run("interrupted after the original was trashed", func(t *testing.T) {
		u := newUser(t)
		a := u.add(t, "photo.jpg", nil)
		status, out := u.upload(t, immichURL, jxl, "photo.jxl", map[string]string{
			"metadata": `[{"key":"immich-optimizer","value":{"v":1,"conversion":{"source":"library","task":{"name":"jpeg-xl"}},"library":{"replaces":"` + a.ID + `","state":"pending"}}}]`})
		if status != http.StatusCreated {
			t.Fatalf("seeding the pending copy: %d %s", status, out["_body"])
		}
		pending := u.waitReady(t, out["id"])
		must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{a.ID}, "force": false}, nil)

		code, logs, log := runLibrary(t, u, sharedDir(t), nil)
		if code != 0 {
			t.Fatalf("library run exited %d:\n%s", code, logs)
		}
		if !contains(events(log, a.ID), "finalized") {
			t.Errorf("events = %v", events(log, ""))
		}
		if m := u.marker(t, pending.ID); m == nil || m["library.state"] != nil {
			t.Errorf("marker = %v", m)
		}
		if u.asset(t, pending.ID).IsTrashed || !u.asset(t, a.ID).IsTrashed {
			t.Error("assets changed while finalizing")
		}
	})
}

// replacedBy returns the id of the copy that replaced an original, from the log.
func replacedBy(log []map[string]any, originalID string) string {
	for _, e := range log {
		if e["event"] == "replaced" && e["asset"] == originalID {
			id, _ := e["new_asset"].(string)
			return id
		}
	}
	return ""
}

func TestLibraryLivePhotosMotionPhotosAndEdits(t *testing.T) {
	u := newUser(t)

	// An Apple live photo: two files Immich pairs by their shared identifier.
	liveVideo := u.add(t, "live.mov", nil)
	live := u.add(t, "live.jpg", nil)
	// An Android motion photo: Immich extracts its video into an asset.
	motion := u.add(t, "motion.jpg", nil)
	// A photo rotated in Immich.
	edited := u.add(t, "photo.jpg", nil)
	edits := map[string]any{"edits": []map[string]any{{"action": "rotate", "parameters": map[string]any{"angle": 90}}}}
	must(t, u.key, http.MethodPut, "/assets/"+edited.ID+"/edits", edits, nil)
	// More assets, so several are worked on at once.
	var plain []asset
	for _, s := range []string{"plain.jpg", "photo.png", "faces/face1.jpg", "faces/face2.jpg", "faces/face3.jpg"} {
		status, out := u.upload(t, immichURL, mediaDir+"/"+s, filepath.Base(s), nil)
		if status != http.StatusCreated {
			t.Fatalf("uploading %s: %d %s", s, status, out["_body"])
		}
		plain = append(plain, u.waitReady(t, out["id"]))
	}

	// Wait until Immich has paired the photos with their videos and applied the edit.
	paired := func(id string) string {
		deadline := time.Now().Add(2 * time.Minute)
		for {
			if a := u.asset(t, id); a.LivePhotoVideoID != nil {
				return *a.LivePhotoVideoID
			}
			if time.Now().After(deadline) {
				t.Fatalf("Immich did not pair asset %s with a video", id)
			}
			time.Sleep(time.Second)
		}
	}
	if got := paired(live.ID); got != liveVideo.ID {
		t.Fatalf("live photo paired with %s, want %s", got, liveVideo.ID)
	}
	motionVideo := paired(motion.ID)
	for deadline := time.Now().Add(time.Minute); !u.asset(t, edited.ID).IsEdited; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("the edit was not applied")
		}
	}

	// With machine learning running, Immich may still be analysing a fresh
	// upload when its turn comes: such an asset is left for the next run.
	// Two runs settle everything; neither may report a failure.
	logDir := sharedDir(t)
	env := map[string]string{"IO_LIBRARY_CONCURRENCY": "4"}
	code, out, _ := runLibrary(t, u, logDir, env)
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	if os.Getenv("E2E_ML") != "" {
		time.Sleep(30 * time.Second)
	}
	code, out, log := runLibrary(t, u, logDir, env)
	if code != 0 {
		t.Fatalf("second library run exited %d:\n%s", code, out)
	}
	for _, e := range log {
		if e["event"] == "failed" {
			t.Errorf("unexpected %v", e)
		}
	}

	// Live photo: the copy is paired with the same, untouched video.
	newLive := replacedBy(log, live.ID)
	if newLive == "" {
		t.Fatalf("live photo not replaced: %v\n%s", events(log, live.ID), out)
	}
	if a := u.asset(t, newLive); a.LivePhotoVideoID == nil || *a.LivePhotoVideoID != liveVideo.ID {
		t.Errorf("live copy paired with %v, want %s", a.LivePhotoVideoID, liveVideo.ID)
	}
	// Motion photo: the photo is converted and paired with the video Immich extracted.
	newMotion := replacedBy(log, motion.ID)
	if newMotion == "" {
		t.Fatalf("motion photo not replaced: %v\n%s", events(log, motion.ID), out)
	}
	if a := u.asset(t, newMotion); a.LivePhotoVideoID == nil || *a.LivePhotoVideoID != motionVideo || a.OriginalMimeType != "image/jxl" {
		t.Errorf("motion copy = %+v, want pairing with %s", a, motionVideo)
	}
	// Edited photo: the copy carries the same edit.
	newEdited := replacedBy(log, edited.ID)
	if newEdited == "" {
		t.Fatalf("edited photo not replaced: %v\n%s", events(log, edited.ID), out)
	}
	var got struct {
		Edits []struct {
			Action     string         `json:"action"`
			Parameters map[string]any `json:"parameters"`
		} `json:"edits"`
	}
	must(t, u.key, http.MethodGet, "/assets/"+newEdited+"/edits", nil, &got)
	if len(got.Edits) != 1 || got.Edits[0].Action != "rotate" || fmt.Sprint(got.Edits[0].Parameters["angle"]) != "90" || !u.asset(t, newEdited).IsEdited {
		t.Errorf("edits on the copy = %+v", got.Edits)
	}
	for _, a := range plain {
		if replacedBy(log, a.ID) == "" || !u.asset(t, a.ID).IsTrashed {
			t.Errorf("%s was not replaced", a.OriginalFileName)
		}
	}

	// After the trash is emptied the videos must still be there, still paired.
	must(t, u.key, http.MethodPost, "/trash/empty", nil, nil)
	deadline := time.Now().Add(2 * time.Minute)
	for call(t, u.key, http.MethodGet, "/assets/"+live.ID, nil, nil) == http.StatusOK {
		if time.Now().After(deadline) {
			t.Fatal("the trash was not emptied")
		}
		time.Sleep(time.Second)
	}
	time.Sleep(3 * time.Second)
	for name, id := range map[string]string{"live video": liveVideo.ID, "motion video": motionVideo} {
		if s := call(t, u.key, http.MethodGet, "/assets/"+id, nil, nil); s != http.StatusOK {
			t.Errorf("the %s is gone after emptying the trash (status %d)", name, s)
		}
	}
	if a := u.asset(t, newLive); a.LivePhotoVideoID == nil || *a.LivePhotoVideoID != liveVideo.ID {
		t.Error("live copy lost its video after emptying the trash")
	}
}

// A phone uploads its photo again once the original has left Immich's trash.
// Library mode must recognise that file as a second copy of something it has
// already converted, at startup and while it runs.
func TestLibrarySecondCopies(t *testing.T) {
	u := newUser(t)
	first := u.add(t, "plain.jpg", map[string]string{"fileCreatedAt": "2015-01-01T10:00:00.000Z"})
	second := u.add(t, "photo.jpg", nil)

	logDir := sharedDir(t)
	code, out, log := runLibrary(t, u, logDir, nil)
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	convFirst, convSecond := replacedBy(log, first.ID), replacedBy(log, second.ID)
	if convFirst == "" || convSecond == "" {
		t.Fatalf("the photos were not converted:\n%s", out)
	}

	// The trash is emptied, and the originals arrive again, straight from a
	// phone. One of them is made a favorite in the meantime.
	must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{first.ID, second.ID}, "force": true}, nil)
	eventually(t, time.Minute, "Immich to delete the originals", func() bool {
		return call(t, u.key, http.MethodGet, "/assets/"+first.ID, nil, nil) >= 400 &&
			call(t, u.key, http.MethodGet, "/assets/"+second.ID, nil, nil) >= 400
	})
	again := u.add(t, "plain.jpg", map[string]string{"fileCreatedAt": "2015-01-01T10:00:00.000Z"})
	favorite := u.add(t, "photo.jpg", nil)
	must(t, u.key, http.MethodPut, "/assets/"+favorite.ID, map[string]any{"isFavorite": true}, nil)
	if again.ID == first.ID || again.Checksum != first.Checksum {
		t.Fatalf("the second upload is not a new asset with the same file: %+v", again)
	}

	// At the next start the second copy goes to the trash, without being
	// converted again.
	code, out, log = runLibrary(t, u, logDir, nil)
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	if !u.asset(t, again.ID).IsTrashed {
		t.Fatalf("the second copy is still in the timeline:\n%s", out)
	}
	trashed := false
	for _, e := range log {
		if e["event"] == "second_copy_trashed" && e["asset"] == again.ID {
			trashed = e["new_asset"] == convFirst
		}
	}
	if !trashed {
		t.Errorf("the audit log does not say which conversion the second copy belongs to: %v", events(log, again.ID))
	}
	// The one with something of its own is left for the user, and the
	// conversions are untouched.
	if u.asset(t, favorite.ID).IsTrashed {
		t.Error("a second copy that is a favorite was trashed, although its conversion is not one")
	}
	for _, id := range []string{convFirst, convSecond} {
		if a := u.asset(t, id); a.IsTrashed || a.OriginalMimeType != "image/jxl" {
			t.Errorf("the conversion %s was touched: %+v", id, a)
		}
	}
	if live := u.live(t); len(live) != 3 {
		t.Errorf("%d assets in the timeline, want the two conversions and the favorite", len(live))
	}

	// While library mode runs, a second copy goes to the trash as soon as
	// it is uploaded: Immich announces the upload with its checksum.
	name, _ := startOptimizer(t, map[string]string{"IO_MODE": "library", "IO_IMMICH_API_KEY": u.keyS}, false, logDir+":/undone")
	waitLibraryReady(t, name, 5*time.Minute)
	must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{again.ID}, "force": true}, nil)
	eventually(t, time.Minute, "Immich to delete the second copy", func() bool {
		return call(t, u.key, http.MethodGet, "/assets/"+again.ID, nil, nil) >= 400
	})
	third := u.add(t, "plain.jpg", map[string]string{"fileCreatedAt": "2015-01-01T10:00:00.000Z"})
	eventually(t, time.Minute, "the copy uploaded while library mode runs to go to the trash", func() bool {
		return u.asset(t, third.ID).IsTrashed
	})

	// While library mode is stopped nothing announces an upload to it. At
	// its next start it asks Immich what arrived meanwhile, and finds the
	// copy without it being tagged.
	if out, err := exec.Command("docker", "stop", "-t", "60", name).CombinedOutput(); err != nil {
		t.Fatalf("docker stop: %v\n%s", err, out)
	}
	must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{third.ID}, "force": true}, nil)
	eventually(t, time.Minute, "Immich to delete the third copy", func() bool {
		return call(t, u.key, http.MethodGet, "/assets/"+third.ID, nil, nil) >= 400
	})
	fourth := u.add(t, "plain.jpg", map[string]string{"fileCreatedAt": "2015-01-01T10:00:00.000Z"})
	code, out, log = runLibrary(t, u, logDir, map[string]string{"E2E_TAGS": "own"})
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	if !u.asset(t, fourth.ID).IsTrashed {
		t.Errorf("the copy uploaded while library mode was stopped is still in the timeline: %v\n%s", events(log, fourth.ID), out)
	}
	if a := u.asset(t, convFirst); a.IsTrashed {
		t.Errorf("the conversion was trashed: %+v", a)
	}
}

// Library mode is driven by tags in Immich: the real tag API, the search by
// tag, and what each tag does.
func TestLibraryTags(t *testing.T) {
	u := newUser(t)
	conv := u.add(t, "plain.jpg", nil)
	kept := u.add(t, "photo.jpg", nil)
	broken := u.add(t, "photo.png", nil)
	untouched := u.add(t, "photo.heic", nil)
	// One of the person's own tags, which has to travel to the copy.
	u.tagWith(t, []string{"holiday", tagConvert}, conv.ID)
	u.tag(t, tagConvert, broken.ID)
	// Tagged keep and convert, as tagging in bulk does: keep protects it.
	u.tagWith(t, []string{tagKeep, tagConvert}, kept.ID)
	// A tag an earlier version used, on no asset: library mode deletes it.
	u.tagID(t, "immich-optimizer/redo")

	// JPEGs convert; the only PNG task fails, so that the failed tag is set.
	cfg := sharedDir(t)
	tasks := `tasks:
- name: jpeg-xl
  command: cjxl --lossless_jpeg=1 -d 0 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
  extensions: [jpg, jpeg]
- name: broken-png
  command: "false"
  extensions: [png]
`
	if err := os.WriteFile(filepath.Join(cfg, "tasks.yaml"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := sharedDir(t)
	run := func() (string, []map[string]any) {
		t.Helper()
		env := map[string]string{"E2E_TAGS": "own", "IO_TASKS_FILE": "/custom/tasks.yaml"}
		code, out, log := runLibrary(t, u, logDir, env, cfg+":/custom:ro")
		if code != 0 {
			t.Fatalf("library run exited %d:\n%s", code, out)
		}
		return out, log
	}
	has := func(id, tag string) bool { return contains(u.tags(t, id), tag) }

	out, log := run()
	// The tags a person uses exist now, under one parent.
	var all []struct {
		Value string `json:"value"`
	}
	must(t, u.key, http.MethodGet, "/tags", nil, &all)
	var values []string
	for _, tg := range all {
		values = append(values, tg.Value)
	}
	for _, want := range []string{tagConvert, tagKeep, tagConvert + "/lossless", tagConvert + "/profile1"} {
		if !contains(values, want) {
			t.Errorf("tag %s does not exist after a run: %v", want, values)
		}
	}
	if contains(values, "immich-optimizer/redo") {
		t.Error("the unused tag immich-optimizer/redo was not deleted")
	}
	if !contains(events(log, ""), "tag_deleted") {
		t.Errorf("the deletion is not in the audit log: %v", events(log, ""))
	}
	newID := replacedBy(log, conv.ID)
	if newID == "" {
		t.Fatalf("the tagged photo was not converted:\n%s", out)
	}
	if got := u.tags(t, newID); !contains(got, "holiday") || contains(got, tagConvert) {
		t.Errorf("tags of the converted copy = %v, want holiday and none of the optimizer's", got)
	}
	if u.marker(t, kept.ID) != nil || u.asset(t, kept.ID).IsTrashed || !has(kept.ID, tagKeep) || has(kept.ID, tagConvert) {
		t.Errorf("keep: marker %v, tags %v", u.marker(t, kept.ID), u.tags(t, kept.ID))
	}
	if !has(broken.ID, tagFailed) || !has(broken.ID, tagConvert) || u.asset(t, broken.ID).IsTrashed {
		t.Errorf("failed conversion: tags %v", u.tags(t, broken.ID))
	}
	if u.marker(t, untouched.ID) != nil || u.asset(t, untouched.ID).IsTrashed {
		t.Error("an untagged asset was touched")
	}

	// Keep removed, the photo is converted when tagged again.
	must(t, u.key, http.MethodDelete, "/tags/"+u.tagID(t, tagKeep)+"/assets", map[string]any{"ids": []string{kept.ID}}, nil)
	// Immich can lose one of two quick tag changes to the same asset: one
	// at a time, as a person makes them.
	eventually(t, time.Minute, "keep to be gone", func() bool {
		time.Sleep(2 * time.Second)
		return !has(kept.ID, tagKeep)
	})
	u.tag(t, tagConvert, kept.ID)
	eventually(t, time.Minute, "convert to be there", func() bool {
		time.Sleep(2 * time.Second)
		return has(kept.ID, tagConvert)
	})
	out, log = run()
	if replacedBy(log, kept.ID) == "" {
		t.Fatalf("after removing keep: %v\n%s", events(log, kept.ID), out)
	}

	// While library mode runs, tagging a photo is enough: Immich tells it.
	name, _ := startOptimizer(t, map[string]string{"IO_MODE": "library", "IO_IMMICH_API_KEY": u.keyS,
		"IO_TASKS_FILE": "/custom/tasks.yaml"}, false, logDir+":/undone", cfg+":/custom:ro")
	waitLibraryReady(t, name, 5*time.Minute)
	live := u.add(t, "live.jpg", nil)
	u.tag(t, tagConvert, live.ID)
	eventually(t, time.Minute, "the photo tagged while library mode runs to be converted", func() bool {
		return u.asset(t, live.ID).IsTrashed
	})
}

// A memory ("on this day") lists its assets by id: the converted copy has to
// take its original's place in it.
func TestLibraryCarriesMemoriesOver(t *testing.T) {
	u := newUser(t)
	a := u.add(t, "photo.jpg", nil)
	b := u.add(t, "plain.jpg", nil)
	var memory struct {
		ID string `json:"id"`
	}
	must(t, u.key, http.MethodPost, "/memories", map[string]any{
		"type": "on_this_day", "data": map[string]any{"year": 2021},
		"memoryAt": "2021-06-15T00:00:00.000Z", "assetIds": []string{a.ID},
	}, &memory)

	code, out, log := runLibrary(t, u, sharedDir(t), nil)
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	newA, newB := replacedBy(log, a.ID), replacedBy(log, b.ID)
	if newA == "" || newB == "" {
		t.Fatalf("the photos were not converted:\n%s", out)
	}
	var got struct {
		Assets []struct {
			ID string `json:"id"`
		} `json:"assets"`
	}
	must(t, u.key, http.MethodGet, "/memories/"+memory.ID, nil, &got)
	var ids []string
	for _, x := range got.Assets {
		ids = append(ids, x.ID)
	}
	if !contains(ids, newA) {
		t.Errorf("the memory does not have the converted photo %s: %v", newA, ids)
	}
	if contains(ids, newB) {
		t.Errorf("a photo that was in no memory was put into one: %v", ids)
	}
}

// One container works for several users, one API key each: each user's
// tags act on that user's photos only.
func TestLibrarySeveralUsers(t *testing.T) {
	alice, bob := newUser(t), newUser(t)
	a := alice.add(t, "plain.jpg", nil)
	aUntagged := alice.add(t, "photo.jpg", nil)
	b := bob.add(t, "photo.jpg", nil)
	logDir := sharedDir(t)
	name, _ := startOptimizer(t, map[string]string{"IO_MODE": "library", "IO_IMMICH_API_KEY": alice.keyS + "," + bob.keyS},
		false, logDir+":/undone")
	eventually(t, 5*time.Minute, "both users' workers to start", func() bool {
		out, _ := exec.Command("docker", "logs", name).CombinedOutput()
		return strings.Contains(string(out), "[user "+alice.id+"] "+libraryReady) &&
			strings.Contains(string(out), "[user "+bob.id+"] "+libraryReady)
	})
	alice.tag(t, tagConvert, a.ID)
	bob.tag(t, tagConvert, b.ID)
	eventually(t, 2*time.Minute, "both tagged photos to be converted", func() bool {
		return alice.asset(t, a.ID).IsTrashed && bob.asset(t, b.ID).IsTrashed
	})
	if alice.asset(t, aUntagged.ID).IsTrashed {
		t.Error("alice's untagged photo was converted")
	}
	// Each replacement is logged with its user.
	users := map[string]string{}
	for _, e := range readLog(t, filepath.Join(logDir, "audit.log")) {
		if e["event"] == "replaced" {
			asset, _ := e["asset"].(string)
			users[asset], _ = e["user"].(string)
		}
	}
	if users[a.ID] != alice.id || users[b.ID] != bob.id {
		t.Errorf("replacements logged for users %v, want alice %s and bob %s", users, alice.id, bob.id)
	}
}

// Conversion on demand only: the proxy passes uploads through, and library
// mode converts what is tagged. The proxy still tells a phone that the
// converted photo is the one it has.
func TestProxyPassesUploadsAndLibraryConvertsOnDemand(t *testing.T) {
	u := newUser(t)
	logDir := sharedDir(t)
	name, proxy := startOptimizer(t, map[string]string{"IO_MODE": "proxy,library", "IO_IMMICH_API_KEY": u.keyS,
		"IO_PROXY_CONVERT_UPLOADS": "false"}, true, logDir+":/undone")
	waitLibraryReady(t, name, 5*time.Minute)

	src := filepath.Join(mediaDir, "photo.jpg")
	status, out := u.upload(t, proxy, src, "photo.jpg", nil)
	if status != http.StatusCreated {
		t.Fatalf("upload through the proxy: %d %s", status, out["_body"])
	}
	a := u.waitReady(t, out["id"])
	if a.OriginalMimeType != "image/jpeg" || a.Checksum != sha1Base64(t, src) {
		t.Fatalf("the upload was changed on its way: %s, checksum %s", a.OriginalMimeType, a.Checksum)
	}

	u.tag(t, tagConvert, a.ID)
	eventually(t, 2*time.Minute, "library mode to convert the tagged photo", func() bool {
		return u.asset(t, a.ID).IsTrashed
	})

	// A phone syncing through the proxy is told the original's checksum.
	found := false
	for _, l := range hiddenLines(syncThrough(t, u, proxy)) {
		found = found || l.Data["checksum"] == a.Checksum
	}
	if !found {
		t.Error("the phone was not told that the converted photo is the one it has")
	}
}

// Converting a conversion again: from the converted file, with every version
// in the record, and the first original still the one phones have.
func TestLibraryConvertAgain(t *testing.T) {
	u := newUser(t)
	a := u.add(t, "plain.jpg", nil)
	cfg := sharedDir(t)
	tasks := `tasks:
- name: jpeg-xl
  command: cjxl --lossless_jpeg=1 -d 0 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
  extensions: [jpg, jpeg]
- name: jxl-again
  command: |
    djxl "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/pixels.png" \
    && cjxl "{{.dst_folder}}/pixels.png" -q 40 "{{.dst_folder}}/{{.name}}.jxl" \
    && rm "{{.dst_folder}}/pixels.png"
  extensions: [jxl]
`
	if err := os.WriteFile(filepath.Join(cfg, "tasks.yaml"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	logDir := sharedDir(t)
	env := map[string]string{"IO_TASKS_FILE": "/custom/tasks.yaml"}

	// First conversion: the JPEG becomes a JPEG XL.
	code, out, log := runLibrary(t, u, logDir, env, cfg+":/custom:ro")
	conv := replacedBy(log, a.ID)
	if code != 0 || conv == "" {
		t.Fatalf("first conversion (exit %d):\n%s", code, out)
	}
	convAsset := u.asset(t, conv)

	// Tagged convert again, it is converted again from the JPEG XL.
	env["E2E_TAGS"] = "own"
	u.tag(t, tagConvert, conv)
	code, out, log = runLibrary(t, u, logDir, env, cfg+":/custom:ro")
	again := replacedBy(log, conv)
	if code != 0 || again == "" {
		t.Fatalf("convert again (exit %d):\n%s", code, out)
	}
	if !u.asset(t, conv).IsTrashed {
		t.Error("the earlier conversion is not in the trash")
	}
	m := u.marker(t, again)
	if m == nil || m["versions.count"] != float64(3) || m["original.sha1"] != hexChecksum(t, a.Checksum) ||
		m["versions.1.sha1"] != hexChecksum(t, convAsset.Checksum) || m["versions.1.task.name"] != "jpeg-xl" ||
		m["latest.task.name"] != "jxl-again" || m["latest.source"] != "library" {
		t.Errorf("record of the version made again: %v", m)
	}
	if contains(u.tags(t, conv), tagConvert) {
		t.Error("the convert tag was not removed")
	}
}

// convert/<profile> converts with that profile: one of your own, mounted in
// the profiles folder, or a bundled one.
func TestLibraryProfiles(t *testing.T) {
	u := newUser(t)
	mine := u.add(t, "plain.jpg", nil)
	bundled := u.add(t, "photo.jpg", nil)
	profiles := sharedDir(t)
	if err := os.MkdirAll(filepath.Join(profiles, "smaller"), 0o755); err != nil {
		t.Fatal(err)
	}
	tasks := `tasks:
- name: small-jxl
  command: cjxl --lossless_jpeg=0 -q 50 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
  extensions: [jpg, jpeg]
`
	if err := os.WriteFile(filepath.Join(profiles, "smaller", "tasks.yaml"), []byte(tasks), 0o644); err != nil {
		t.Fatal(err)
	}
	u.tag(t, tagConvert+"/smaller", mine.ID)
	u.tag(t, tagConvert+"/lossless", bundled.ID)
	code, out, log := runLibrary(t, u, sharedDir(t), map[string]string{"E2E_TAGS": "own"}, profiles+":/etc/immich-optimizer/profiles:ro")
	listed := regexp.MustCompile(`Library mode: profiles ([^(]*) \(`).FindStringSubmatch(out)
	if code != 0 || len(listed) < 2 || !strings.Contains(listed[1], "lossless") || !strings.Contains(listed[1], "smaller") {
		t.Fatalf("library mode (exit %d) did not list the profiles:\n%s", code, out)
	}
	for _, c := range []struct {
		a    asset
		task string
	}{{mine, "small-jxl"}, {bundled, "jpeg-xl"}} {
		id := replacedBy(log, c.a.ID)
		if id == "" {
			t.Fatalf("%s was not converted:\n%s", c.a.OriginalFileName, out)
		}
		if m := u.marker(t, id); m == nil || m["latest.task.name"] != c.task {
			t.Errorf("%s: converted with %v, want %s", c.a.OriginalFileName, m["latest.task.name"], c.task)
		}
	}
	// Only the tag the photo had is recorded as removed: Immich's search
	// for plain convert also finds it, but it never had that tag.
	var removed []string
	for _, e := range log {
		if e["event"] == "tag_removed" && e["asset"] == mine.ID {
			removed = append(removed, fmt.Sprint(e["detail"]))
		}
	}
	if len(removed) != 1 || removed[0] != tagConvert+"/smaller" {
		t.Errorf("tag removals logged for %s: %v", mine.OriginalFileName, removed)
	}
}
