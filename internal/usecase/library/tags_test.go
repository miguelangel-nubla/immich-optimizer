package library

import (
	"errors"
	"strings"
	"testing"
)

// tagged returns a harness in which only what a test tags counts.
func tagged(t *testing.T) *harness {
	h := newHarness(t)
	h.im.convertAll = false
	return h
}

// convertedFrom returns the live asset whose marker names old as replaced.
func (h *harness) convertedFrom(old *fakeAsset) *fakeAsset {
	for _, id := range h.im.order {
		if m := h.marker(id); m != nil && m.Replaces == old.ID {
			return h.im.assets[id]
		}
	}
	return nil
}

func TestOnlyTaggedAssetsAreConverted(t *testing.T) {
	h := tagged(t)
	in := h.im.add("IMG_1.jpg", photo(1))
	out := h.im.add("IMG_2.jpg", photo(2))
	h.im.tag(in, TagConvert)
	in.TagIDs = append(in.TagIDs, "tag-1") // one of the person's own tags

	sum, err := h.run()
	if err != nil || sum.Replaced != 1 || sum.Skipped != 1 || !in.IsTrashed || out.IsTrashed {
		t.Fatalf("%+v, %v", sum, err)
	}
	n := h.convertedFrom(in)
	if n == nil {
		t.Fatal("no converted copy")
	}
	// The person's tag travels to the copy; the command does not.
	if h.im.hasTag(n, TagConvert) || len(n.TagIDs) != 1 || n.TagIDs[0] != "tag-1" {
		t.Errorf("tags of the copy = %v", n.TagIDs)
	}
	// The command is settled on the original.
	if h.im.hasTag(in, TagConvert) {
		t.Error("the original still has the convert tag")
	}
	// Nothing is tagged any more: the next run converts nothing.
	h.im.calls = nil
	if sum, err = h.run(); err != nil || sum.Replaced != 0 || len(h.im.calls) != 0 {
		t.Errorf("second run: %+v, %v, %v", sum, err, h.im.calls)
	}
}

func TestNothingTaggedConvertsNothing(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	sum, err := h.run()
	if err != nil || sum.Replaced != 0 || sum.Skipped != 1 || a.IsTrashed || len(h.im.calls) != 0 {
		t.Fatalf("%+v, %v, %v", sum, err, h.im.calls)
	}
}

// The command tags exist from the first run on, even a dry run, so that they
// can be found in Immich. The status tags only appear once they are needed.
func TestCommandTagsAreCreated(t *testing.T) {
	h := tagged(t)
	h.cfg.DryRun = true
	h.im.add("IMG_1.jpg", photo(1))
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{TagConvert, TagDone, TagRedo} {
		if h.im.tags[v] == "" {
			t.Errorf("%s was not created", v)
		}
	}
	for _, v := range []string{TagFailed, TagReview} {
		if h.im.tags[v] != "" {
			t.Errorf("%s was created without being needed", v)
		}
	}
	// A second run creates nothing more.
	h.im.tagCalls = nil
	if _, err := h.run(); err != nil || len(h.im.tagCalls) != 0 {
		t.Errorf("second run: %v, %v", err, h.im.tagCalls)
	}
}

func TestDoneMarksWithoutConverting(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.mkv", photo(1)) // a type no task converts: done works anyway
	b := h.im.add("IMG_2.jpg", photo(2))
	h.im.tag(a, TagDone)
	h.im.tag(b, TagDone)
	h.im.tag(b, TagConvert) // done wins

	// A dry run only says so.
	h.cfg.DryRun = true
	sum, err := h.run()
	if err != nil || sum.WouldMarkDone != 2 || sum.MarkedDone != 0 || h.marker(a.ID) != nil || !h.im.hasTag(a, TagDone) {
		t.Fatalf("dry run: %+v, %v", sum, err)
	}
	h.cfg.DryRun = false

	sum, err = h.run()
	if err != nil || sum.MarkedDone != 2 || sum.Replaced != 0 {
		t.Fatalf("%+v, %v", sum, err)
	}
	for _, x := range []*fakeAsset{a, b} {
		m := h.marker(x.ID)
		if m == nil || !m.Kept || m.Note != DoneNote || m.SHA1 != x.Checksum {
			t.Errorf("%s: marker %+v", x.OriginalFileName, m)
		}
		if x.IsTrashed || h.im.hasTag(x, TagDone) || h.im.hasTag(x, TagConvert) {
			t.Errorf("%s: trashed %v, tags %v", x.OriginalFileName, x.IsTrashed, x.TagIDs)
		}
	}
	if n := strings.Count(strings.Join(h.im.calls, " "), "upload:"); n != 0 {
		t.Errorf("%d uploads: done converts nothing", n)
	}
	ev := strings.Join(h.events(), " ")
	if !strings.Contains(ev, "would_mark_done") || !strings.Contains(ev, "marked_done") {
		t.Errorf("events: %s", ev)
	}

	// Tagging it to convert later does nothing: it is done.
	h.im.tag(b, TagConvert)
	if sum, err = h.run(); err != nil || sum.Replaced != 0 || b.IsTrashed || h.im.hasTag(b, TagConvert) {
		t.Errorf("convert after done: %+v, %v", sum, err)
	}
}

// Done on an asset the optimizer already converted leaves its marker as it
// is: that marker carries the original's checksum.
func TestDoneOnAConversionKeepsItsMarker(t *testing.T) {
	h := newHarness(t)
	_, conv := convertOne(t, h, photo(1))
	h.im.convertAll = false
	before := string(conv.metadata["immich-optimizer"])
	h.im.tag(conv, TagDone)
	sum, err := h.run()
	if err != nil || sum.MarkedDone != 0 || string(conv.metadata["immich-optimizer"]) != before || h.im.hasTag(conv, TagDone) {
		t.Fatalf("%+v, %v, marker %s", sum, err, conv.metadata["immich-optimizer"])
	}
}

func TestRedoRemovesADoneMarkAndAllowsConverting(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagDone)
	if _, err := h.run(); err != nil || h.marker(a.ID) == nil {
		t.Fatalf("done: %v", err)
	}

	h.im.tag(a, TagRedo)
	h.im.tag(a, TagConvert)
	// A dry run says it would remove the mark, and removes nothing.
	h.cfg.DryRun = true
	sum, err := h.run()
	if err != nil || sum.WouldForget != 1 || h.marker(a.ID) == nil || !h.im.hasTag(a, TagRedo) {
		t.Fatalf("dry run: %+v, %v", sum, err)
	}
	h.cfg.DryRun = false

	// The mark goes, and the same run converts it.
	sum, err = h.run()
	if err != nil || sum.Forgotten != 1 || sum.Replaced != 1 || !a.IsTrashed {
		t.Fatalf("%+v, %v", sum, err)
	}
	if h.im.hasTag(a, TagRedo) || h.im.hasTag(a, TagConvert) {
		t.Errorf("tags left: %v", a.TagIDs)
	}
	if !strings.Contains(strings.Join(h.events(), " "), "marker_removed") {
		t.Errorf("events: %v", h.events())
	}
}

// Redo only forgets a "left as it is" mark. A conversion keeps its marker:
// without it, phones would upload the original again.
func TestRedoNeverRemovesAConversionsMarker(t *testing.T) {
	h := newHarness(t)
	_, conv := convertOne(t, h, photo(1))
	h.im.convertAll = false
	h.im.tag(conv, TagRedo)
	h.im.tag(conv, TagConvert)
	sum, err := h.run()
	if err != nil || sum.Forgotten != 0 || sum.Replaced != 0 || h.marker(conv.ID) == nil {
		t.Fatalf("%+v, %v", sum, err)
	}
	if h.im.hasTag(conv, TagRedo) || h.im.hasTag(conv, TagConvert) {
		t.Errorf("tags left: %v", conv.TagIDs)
	}
}

func TestFailedConversionIsTaggedFailed(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagConvert)
	h.conv.convertErr = errors.New("encoder crashed")
	sum, err := h.run()
	if err != nil || sum.Failed != 1 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if !h.im.hasTag(a, TagFailed) || h.im.hasTag(a, TagConvert) {
		t.Errorf("tags after a failure: %v", a.TagIDs)
	}
	// Not tried again until it is tagged again.
	h.conv.convertErr = nil
	if sum, err = h.run(); err != nil || sum.Failed != 0 || sum.Replaced != 0 {
		t.Errorf("untagged: %+v, %v", sum, err)
	}
	// Tagged again, it does not shrink this time and is kept: the failed
	// tag goes, since it is settled now.
	h.conv.noGain = true
	h.im.tag(a, TagConvert)
	if sum, err = h.run(); err != nil || sum.Kept != 1 {
		t.Fatalf("retry: %+v, %v", sum, err)
	}
	if h.im.hasTag(a, TagFailed) || h.im.hasTag(a, TagConvert) {
		t.Errorf("tags after the retry: %v", a.TagIDs)
	}
}

// A failed asset that is then converted leaves the failed tag on the
// original in the trash; the converted copy does not get it.
func TestFailedTagIsNotCarriedOver(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagFailed)
	h.im.tag(a, TagConvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 1 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if n := h.convertedFrom(a); n == nil || h.im.hasTag(n, TagFailed) {
		t.Errorf("the copy has the failed tag: %+v", n)
	}
}

func TestSecondCopyLeftAloneIsTaggedForReview(t *testing.T) {
	h := newHarness(t)
	data := photo(1)
	old, _ := convertOne(t, h, data)
	old.deleted = true
	again := h.im.add("IMG_1.jpg", data)
	again.Description = "mine"
	if sum, err := h.run(); err != nil || sum.SecondCopies != 0 || again.IsTrashed {
		t.Fatalf("%+v, %v", sum, err)
	}
	if !h.im.hasTag(again, TagReview) {
		t.Errorf("tags = %v", again.TagIDs)
	}
}

// A reason that may go away by itself leaves the convert tag, so that a
// later run tries again.
func TestTransientReasonKeepsTheConvertTag(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	a.IsOffline = true
	h.im.tag(a, TagConvert)
	if _, err := h.run(); err != nil || !h.im.hasTag(a, TagConvert) {
		t.Fatalf("%v, tags %v", err, a.TagIDs)
	}
	a.IsOffline = false
	if sum, err := h.run(); err != nil || sum.Replaced != 1 || h.im.hasTag(a, TagConvert) {
		t.Fatalf("once online: %+v, %v", sum, err)
	}
}

// A type no task converts is settled: the tag goes, the report says why.
func TestTaggedTypeWithoutATaskLosesTheTag(t *testing.T) {
	h := tagged(t)
	a := h.im.add("notes.txt", photo(1))
	h.im.tag(a, TagConvert)
	if sum, err := h.run(); err != nil || sum.Skipped != 1 || h.im.hasTag(a, TagConvert) {
		t.Fatalf("%+v, %v, tags %v", sum, err, a.TagIDs)
	}
}

// Tags are only settled at the end of a real run. A dry run leaves them.
func TestDryRunLeavesTheTags(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagConvert)
	h.cfg.DryRun = true
	sum, err := h.run()
	if err != nil || sum.WouldReplace != 1 || !h.im.hasTag(a, TagConvert) {
		t.Fatalf("%+v, %v", sum, err)
	}
}

// Not being able to read the tags stops the run before any change.
func TestTagsThatCannotBeReadStopTheRun(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagConvert)
	h.im.failCall["TaggedAssetIDs"] = errors.New("immich POST /search/metadata: status 500")
	if _, err := h.run(); err == nil || !strings.Contains(err.Error(), TagConvert) || a.IsTrashed || len(h.im.calls) != 0 {
		t.Fatalf("err = %v, calls %v", err, h.im.calls)
	}
}

// A tag that cannot be removed is not an error: the next run finds the work
// done and removes it then.
func TestTagThatCannotBeRemovedIsRemovedNextRun(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.conv.noGain = true
	h.im.tag(a, TagConvert)
	h.im.failCall["UntagAssets"] = errors.New("immich DELETE /tags: status 500")
	if sum, err := h.run(); err != nil || sum.Kept != 1 || !h.im.hasTag(a, TagConvert) {
		t.Fatalf("%+v, %v", sum, err)
	}
	delete(h.im.failCall, "UntagAssets")
	if sum, err := h.run(); err != nil || sum.Kept != 0 || h.im.hasTag(a, TagConvert) {
		t.Fatalf("next run: %+v, %v, tags %v", sum, err, a.TagIDs)
	}
}

// The unfinished copy of an interrupted replacement is settled first, also
// when it carries a tag; the tag waits for a later run.
func TestTaggedUnfinishedCopyIsSettledFirst(t *testing.T) {
	h := tagged(t)
	old := h.im.add("IMG_1.jpg", photo(1))
	old.IsTrashed = true // the interrupted run got as far as the trash
	n := pendingCopy(h, old)
	h.im.tag(n, TagDone)
	sum, err := h.run()
	if err != nil || sum.Finalized != 1 || sum.MarkedDone != 0 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if m := h.marker(n.ID); m == nil || m.State != "" || m.Kept {
		t.Errorf("marker after finishing = %+v", m)
	}
	if !h.im.hasTag(n, TagDone) {
		t.Error("the tag was removed before it was acted on")
	}
	// The next run acts on it: the copy is a conversion, so its marker stays.
	if sum, err = h.run(); err != nil || h.im.hasTag(n, TagDone) {
		t.Fatalf("next run: %+v, %v", sum, err)
	}
	if m := h.marker(n.ID); m == nil || m.Kept || m.Replaces != old.ID {
		t.Errorf("the conversion's marker changed: %+v", m)
	}
}
