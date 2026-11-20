//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

	// A dry run (the default) reports and changes nothing.
	code, out, log := runLibrary(t, u, logDir, nil)
	if code != 0 {
		t.Fatalf("dry run exited %d:\n%s", code, out)
	}
	if !contains(events(log, r.a.ID), "would_convert") {
		t.Errorf("dry run did not log would_convert: %v", events(log, ""))
	}
	if n := len(u.live(t)); n != 2 { // the photo and its stack partner
		t.Fatalf("dry run changed the library: %d live assets", n)
	}
	if u.asset(t, r.a.ID).IsTrashed || u.marker(t, r.a.ID) != nil {
		t.Fatal("dry run touched the original")
	}

	// The real run.
	code, out, log = runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
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
	if m == nil || m["replaces"] != r.a.ID || m["sha1"] != hexChecksum(t, r.a.Checksum) || m["source"] != "library" || m["state"] != nil {
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
	code, out, log2 := runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
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
	code, out, _ = runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
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
	code, out, log := runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
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
	env := map[string]string{"IO_LIBRARY_DRY_RUN": "false"}
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
			"metadata": `[{"key":"immich-optimizer","value":{"v":1,"sha1":"` + hexChecksum(t, a.Checksum) + `","task":"jpeg-xl","source":"library","replaces":"` + a.ID + `","state":"pending"}}]`})
		if status != http.StatusCreated {
			t.Fatalf("seeding the pending copy: %d %s", status, out["_body"])
		}
		pending := u.waitReady(t, out["id"])

		code, logs, log := runLibrary(t, u, sharedDir(t), map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
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
		if m := u.marker(t, live[0].ID); m == nil || m["state"] != nil || m["replaces"] != a.ID {
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
			"metadata": `[{"key":"immich-optimizer","value":{"v":1,"task":"jpeg-xl","source":"library","replaces":"` + a.ID + `","state":"pending"}}]`})
		if status != http.StatusCreated {
			t.Fatalf("seeding the pending copy: %d %s", status, out["_body"])
		}
		pending := u.waitReady(t, out["id"])
		must(t, u.key, http.MethodDelete, "/assets", map[string]any{"ids": []string{a.ID}, "force": false}, nil)

		code, logs, log := runLibrary(t, u, sharedDir(t), map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
		if code != 0 {
			t.Fatalf("library run exited %d:\n%s", code, logs)
		}
		if !contains(events(log, a.ID), "finalized") {
			t.Errorf("events = %v", events(log, ""))
		}
		if m := u.marker(t, pending.ID); m == nil || m["state"] != nil {
			t.Errorf("marker = %v", m)
		}
		if u.asset(t, pending.ID).IsTrashed || !u.asset(t, a.ID).IsTrashed {
			t.Error("assets changed while finalizing")
		}
	})
}

func TestLibraryDateFilterAndLimit(t *testing.T) {
	u := newUser(t)
	// plain.jpg has no date inside, so Immich uses the given file date;
	// photo.jpg says 2021 in its metadata.
	oldPhoto := u.add(t, "plain.jpg", map[string]string{"fileCreatedAt": "2015-01-01T10:00:00.000Z"})
	newPhoto := u.add(t, "photo.jpg", nil)

	logDir := sharedDir(t)
	code, out, _ := runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false", "IO_LIBRARY_TAKEN_BEFORE": "2020-01-01"})
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	if !u.asset(t, oldPhoto.ID).IsTrashed {
		t.Error("the old photo was not converted")
	}
	if u.asset(t, newPhoto.ID).IsTrashed {
		t.Error("a photo newer than the date was converted")
	}

	// A bad date stops the optimizer at startup instead of converting everything.
	code, out, _ = runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false", "IO_LIBRARY_TAKEN_BEFORE": "last year"})
	if code == 0 || !strings.Contains(out, "library_taken_before") {
		t.Errorf("bad date accepted (exit %d):\n%s", code, out)
	}
	if u.asset(t, newPhoto.ID).IsTrashed {
		t.Error("a bad date converted the rest")
	}
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
	env := map[string]string{"IO_LIBRARY_DRY_RUN": "false", "IO_LIBRARY_CONCURRENCY": "4"}
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

// readReport reads the data embedded in a library report page.
func readReport(t *testing.T, path string) (map[string]any, string) {
	t.Helper()
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no report: %v", err)
	}
	const open, end = "window.REPORT = ", ";</script>"
	i := strings.Index(string(page), open)
	if i < 0 {
		t.Fatal("no data in the report page")
	}
	j := strings.Index(string(page[i:]), end)
	var data map[string]any
	if err := json.Unmarshal(page[i+len(open):i+j], &data); err != nil {
		t.Fatalf("report data is not JSON: %v", err)
	}
	if copyTo := os.Getenv("E2E_REPORT_COPY"); copyTo != "" {
		kind := "run"
		if data["dry_run"] == true {
			kind = "dry-run"
		}
		_ = os.WriteFile(filepath.Join(copyTo, filepath.Base(t.Name())+"-"+kind+".html"), page, 0o644)
	}
	return data, string(page)
}

func TestLibraryDryRunReport(t *testing.T) {
	u := newUser(t)
	samples := []string{"photo.jpg", "plain.jpg", "photo.png", "faces/face1.jpg", "faces/face2.jpg", "photo.heic", "clip.mp4"}
	for _, s := range samples {
		status, out := u.upload(t, immichURL, mediaDir+"/"+s, filepath.Base(s), nil)
		if status != http.StatusCreated {
			t.Fatalf("uploading %s: %d %s", s, status, out["_body"])
		}
		u.waitReady(t, out["id"])
	}
	logDir := sharedDir(t)
	code, out, _ := runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN_SAMPLE": "3", "IO_IMMICH_PUBLIC_URL": "https://photos.example.com"})
	if code != 0 {
		t.Fatalf("dry run exited %d:\n%s", code, out)
	}
	data, _ := readReport(t, filepath.Join(logDir, "library-report.html"))
	if data["dry_run"] != true || data["public_url"] != "https://photos.example.com" {
		t.Errorf("report header = %v %v", data["dry_run"], data["public_url"])
	}
	items := data["items"].([]any)
	if len(items) != len(samples) {
		t.Fatalf("%d items in the report, want %d", len(items), len(samples))
	}
	would, sampled := 0, 0
	for _, raw := range items {
		it := raw.(map[string]any)
		if it["status"] == "would_convert" {
			would++
		}
		if it["status"] == "skipped" && it["reason"] == nil {
			t.Errorf("skipped %v has no reason", it["file"])
		}
		if sm, ok := it["sample"].(map[string]any); ok {
			sampled++
			before, _ := sm["before"].(string)
			after, _ := sm["after"].(string)
			if sm["result"] != "smaller" || !strings.HasPrefix(before, "data:image/jpeg;base64,") || !strings.HasPrefix(after, "data:image/jpeg;base64,") {
				t.Errorf("sample of %v: result %v, previews %d/%d bytes", it["file"], sm["result"], len(before), len(after))
			}
		}
	}
	if would != 5 || sampled != 3 {
		t.Errorf("would=%d sampled=%d, want 5 and 3", would, sampled)
	}
	est, _ := data["estimate"].(map[string]any)
	if est == nil || est["sampled"].(float64) != 3 || est["estimated_after"].(float64) >= est["candidate_bytes"].(float64) {
		t.Errorf("estimate = %v", est)
	}
	// A dry run with a sample still changes nothing.
	if n := len(u.live(t)); n != len(samples) {
		t.Errorf("%d live assets after the dry run, want %d", n, len(samples))
	}
	for _, a := range u.live(t) {
		if u.marker(t, a.ID) != nil {
			t.Errorf("%s got a marker in a dry run", a.OriginalFileName)
		}
	}

	// A real run rewrites the report with what was done.
	code, out, _ = runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}
	data, _ = readReport(t, filepath.Join(logDir, "library-report.html"))
	replaced := 0
	for _, raw := range data["items"].([]any) {
		it := raw.(map[string]any)
		if it["status"] == "replaced" && it["new_id"] != nil {
			replaced++
		}
	}
	if data["dry_run"] == true || replaced != 5 {
		t.Errorf("real run report: dry_run=%v replaced=%d", data["dry_run"], replaced)
	}
}

// A phone uploads its photo again once the original has left Immich's trash.
// Library mode must recognise that file as a second copy of something it has
// already converted, and must keep doing so on a schedule.
func TestLibrarySecondCopiesAndSchedule(t *testing.T) {
	u := newUser(t)
	first := u.add(t, "plain.jpg", map[string]string{"fileCreatedAt": "2015-01-01T10:00:00.000Z"})
	second := u.add(t, "photo.jpg", nil)

	logDir := sharedDir(t)
	code, out, log := runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
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

	// A dry run says what it would do and does nothing.
	code, out, log = runLibrary(t, u, logDir, nil)
	if code != 0 || u.asset(t, again.ID).IsTrashed {
		t.Fatalf("dry run (exit %d) trashed something:\n%s", code, out)
	}
	if !contains(events(log, again.ID), "would_trash_second_copy") {
		t.Errorf("the dry run did not report the second copy: %v", events(log, again.ID))
	}

	// A real run, limited to old photos: the second copy goes to the trash
	// whatever the limit, without being converted again.
	code, out, log = runLibrary(t, u, logDir, map[string]string{"IO_LIBRARY_DRY_RUN": "false", "IO_LIBRARY_OLDER_THAN": "1h"})
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

	// With an interval it does not exit: it runs again and again.
	before := len(events(readLog(t, filepath.Join(logDir, "audit.log")), ""))
	startOptimizer(t, map[string]string{"IO_MODE": "library", "IO_IMMICH_API_KEY": u.keyS, "IO_LIBRARY_INTERVAL": "2s",
		"IO_LIBRARY_DRY_RUN_SAMPLE": "0"}, false, logDir+":/undone")
	eventually(t, 2*time.Minute, "three scheduled runs", func() bool {
		n := 0
		for _, e := range events(readLog(t, filepath.Join(logDir, "audit.log")), "")[before:] {
			if e == "run_finished" {
				n++
			}
		}
		return n >= 3
	})
}
