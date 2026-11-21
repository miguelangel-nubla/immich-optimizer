package library

import (
	"encoding/json"
	"errors"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
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

// The command tags exist from the first run on, so that they can be found
// in Immich. The status tags only appear once they are needed.
func TestCommandTagsAreCreated(t *testing.T) {
	h := tagged(t)
	h.im.add("IMG_1.jpg", photo(1))
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{TagConvert, TagKeep, TagRedo} {
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

func TestKeepMarksWithoutConverting(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.mkv", photo(1)) // a type no task converts: keep works anyway
	b := h.im.add("IMG_2.jpg", photo(2))
	h.im.tag(a, TagKeep)
	h.im.tag(b, TagKeep)
	h.im.tag(b, TagConvert) // keep wins

	sum, err := h.run()
	if err != nil || sum.MarkedKeep != 2 || sum.Replaced != 0 {
		t.Fatalf("%+v, %v", sum, err)
	}
	for _, x := range []*fakeAsset{a, b} {
		m := h.marker(x.ID)
		if m == nil || !m.Kept || m.Note != KeepNote || m.SHA1 != x.Checksum {
			t.Errorf("%s: marker %+v", x.OriginalFileName, m)
		}
		if x.IsTrashed || h.im.hasTag(x, TagKeep) || h.im.hasTag(x, TagConvert) {
			t.Errorf("%s: trashed %v, tags %v", x.OriginalFileName, x.IsTrashed, x.TagIDs)
		}
	}
	if n := strings.Count(strings.Join(h.im.calls, " "), "upload:"); n != 0 {
		t.Errorf("%d uploads: keep converts nothing", n)
	}
	ev := strings.Join(h.events(), " ")
	if !strings.Contains(ev, "marked_keep") {
		t.Errorf("events: %s", ev)
	}

	// Tagging it to convert later does nothing: it is kept.
	h.im.tag(b, TagConvert)
	if sum, err = h.run(); err != nil || sum.Replaced != 0 || b.IsTrashed || h.im.hasTag(b, TagConvert) {
		t.Errorf("convert after keep: %+v, %v", sum, err)
	}
}

// Keep on an asset the optimizer already converted leaves its marker as it
// is: that marker carries the original's checksum.
func TestKeepOnAConversionKeepsItsMarker(t *testing.T) {
	h := newHarness(t)
	_, conv := convertOne(t, h, photo(1))
	h.im.convertAll = false
	before := string(conv.metadata["immich-optimizer"])
	h.im.tag(conv, TagKeep)
	sum, err := h.run()
	if err != nil || sum.MarkedKeep != 0 || string(conv.metadata["immich-optimizer"]) != before || h.im.hasTag(conv, TagKeep) {
		t.Fatalf("%+v, %v, marker %s", sum, err, conv.metadata["immich-optimizer"])
	}
}

func TestRedoRemovesAKeepMarkAndAllowsConverting(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagKeep)
	if _, err := h.run(); err != nil || h.marker(a.ID) == nil {
		t.Fatalf("keep: %v", err)
	}

	h.im.tag(a, TagRedo)
	h.im.tag(a, TagConvert)
	// The mark goes, and the same run converts it.
	sum, err := h.run()
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
	h.im.tag(n, TagKeep)
	sum, err := h.run()
	if err != nil || sum.Finalized != 1 || sum.MarkedKeep != 0 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if m := h.marker(n.ID); m == nil || m.State != "" || m.Kept {
		t.Errorf("marker after finishing = %+v", m)
	}
	if !h.im.hasTag(n, TagKeep) {
		t.Error("the tag was removed before it was acted on")
	}
	// The next run acts on it: the copy is a conversion, so its marker stays.
	if sum, err = h.run(); err != nil || h.im.hasTag(n, TagKeep) {
		t.Fatalf("next run: %+v, %v", sum, err)
	}
	if m := h.marker(n.ID); m == nil || m.Kept || m.Replaces != old.ID {
		t.Errorf("the conversion's marker changed: %+v", m)
	}
}

// Converting a conversion again: from the converted file, with the record of
// every version kept, and the first original still the one phones have.
func TestReconvertAppendsAVersion(t *testing.T) {
	h := newHarness(t)
	data := photo(1)
	old, conv := convertOne(t, h, data)
	h.im.convertAll = false
	h.conv.jxlToo = true
	first := h.marker(conv.ID)

	// A plain convert tag leaves a conversion alone.
	h.im.tag(conv, TagConvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 0 || conv.IsTrashed {
		t.Fatalf("convert on a conversion: %+v, %v", sum, err)
	}

	h.im.tag(conv, TagReconvert)
	sum, err := h.run()
	if err != nil || sum.Replaced != 1 || !conv.IsTrashed {
		t.Fatalf("reconvert: %+v, %v", sum, err)
	}
	again := h.convertedFrom(conv)
	if again == nil {
		t.Fatal("no new copy")
	}
	m := h.marker(again.ID)
	if m.SHA1 != old.Checksum || m.Size != first.Size || m.OriginalName != first.OriginalName {
		t.Errorf("the original changed: %+v", m)
	}
	if len(m.Earlier) != 1 || m.Earlier[0].SHA1 != conv.Checksum || m.Earlier[0].Size != conv.Size || m.Earlier[0].Task != "jpeg-xl" {
		t.Errorf("the earlier conversion was not kept: %+v", m.Earlier)
	}
	if m.OutputSHA1 != again.Checksum && m.OutputSHA1 != "" || m.State != "" || m.Replaces != conv.ID {
		t.Errorf("latest version: %+v", m)
	}
	if h.im.hasTag(conv, TagReconvert) || h.im.hasTag(again, TagReconvert) {
		t.Error("the reconvert tag was not settled")
	}
	// The audit log ties the new asset to the first original, as the
	// second-copy check and the phones need.
	found := false
	for _, e := range h.entries() {
		if e.Event == "replaced" && e.NewAsset == again.ID {
			found = e.OriginalSHA1 == old.Checksum && strings.Contains(e.Detail, "converted again")
		}
	}
	if !found {
		t.Error("the replacement is not logged against the first original")
	}

	// Once more: a third version.
	h.im.tag(again, TagReconvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 1 {
		t.Fatalf("third version: %+v, %v", sum, err)
	}
	third := h.convertedFrom(again)
	if m := h.marker(third.ID); m == nil || len(m.Earlier) != 2 || m.SHA1 != old.Checksum {
		t.Errorf("third version record: %+v", m)
	}
}

// When converting again gives nothing better, the asset and its record stay
// exactly as they are; no "kept" mark replaces its history.
func TestReconvertWithoutGainChangesNothing(t *testing.T) {
	h := newHarness(t)
	_, conv := convertOne(t, h, photo(1))
	h.im.convertAll = false
	h.conv.jxlToo, h.conv.noGain = true, true
	before := string(conv.metadata[entity.MarkerKey])
	h.im.tag(conv, TagReconvert)
	sum, err := h.run()
	if err != nil || sum.Replaced != 0 || sum.Kept != 0 || sum.Skipped != 1 || conv.IsTrashed {
		t.Fatalf("%+v, %v", sum, err)
	}
	if string(conv.metadata[entity.MarkerKey]) != before {
		t.Errorf("the record changed:\n%s\nwas\n%s", conv.metadata[entity.MarkerKey], before)
	}
	if h.im.hasTag(conv, TagReconvert) {
		t.Error("the tag was not settled")
	}
}

// Reconvert on something kept as it is says how to convert it instead; on
// something never converted it converts it.
func TestReconvertOnKeptAndOnUnconverted(t *testing.T) {
	h := tagged(t)
	kept := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(kept, TagKeep)
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	before := string(kept.metadata[entity.MarkerKey])
	h.im.tag(kept, TagReconvert)
	plain := h.im.add("IMG_2.jpg", photo(2))
	h.im.tag(plain, TagReconvert)
	sum, err := h.run()
	if err != nil || sum.Replaced != 1 || !plain.IsTrashed || kept.IsTrashed {
		t.Fatalf("%+v, %v", sum, err)
	}
	if string(kept.metadata[entity.MarkerKey]) != before || h.im.hasTag(kept, TagReconvert) {
		t.Errorf("kept asset: marker %s, tags %v", kept.metadata[entity.MarkerKey], kept.TagIDs)
	}
}

// A record from an earlier version, which does not say what the original
// was, is not built on: phones would lose the photo.
func TestReconvertNeedsAReadableRecord(t *testing.T) {
	h := tagged(t)
	h.conv.jxlToo = true
	a := h.im.add("IMG_1.jxl", photo(1))
	old := `{"v":1,"original":{"sha1":"aa","size":1},"conversion":{"task":{"name":"jpeg-xl"}}}`
	a.metadata[entity.MarkerKey] = json.RawMessage(old)
	h.im.tag(a, TagReconvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 0 || sum.Skipped != 1 || a.IsTrashed || string(a.metadata[entity.MarkerKey]) != old {
		t.Fatalf("%+v, %v", sum, err)
	}
}
