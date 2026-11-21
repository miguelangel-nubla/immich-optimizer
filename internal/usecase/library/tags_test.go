package library

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
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

func TestFailedConversionIsTaggedFailed(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagConvert)
	h.conv.convertErr = errors.New("encoder crashed")
	sum, err := h.run()
	if err != nil || sum.Failed != 1 {
		t.Fatalf("%+v, %v", sum, err)
	}
	// Failed is added; convert stays, as the record of what was asked.
	if !h.im.hasTag(a, TagFailed) || !h.im.hasTag(a, TagConvert) {
		t.Errorf("tags after a failure: %v", a.TagIDs)
	}
	// Not tried again while it is tagged failed.
	h.conv.convertErr = nil
	if sum, err = h.run(); err != nil || sum.Failed != 0 || sum.Replaced != 0 || !h.im.hasTag(a, TagConvert) {
		t.Errorf("while tagged failed: %+v, %v", sum, err)
	}
	// Removing failed is how to try again.
	if err := h.im.UntagAssets(context.Background(), h.im.tags[TagFailed], []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	if sum, err = h.run(); err != nil || sum.Replaced != 1 || !a.IsTrashed {
		t.Fatalf("after removing failed: %+v, %v", sum, err)
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

// A tag that cannot be removed is not an error: the next run acts on it
// again and removes it then.
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
	// Still tagged, it is tried again; this time the tag goes.
	if sum, err := h.run(); err != nil || sum.Kept != 1 || h.im.hasTag(a, TagConvert) {
		t.Fatalf("next run: %+v, %v, tags %v", sum, err, a.TagIDs)
	}
}

// The command tags exist from the first run on, so that they can be found
// in Immich: convert, one per profile, and keep. The status tags only
// appear once they are needed.
func TestCommandTagsAreCreated(t *testing.T) {
	h := tagged(t)
	h.cfg.Profiles = map[string]port.CheckingConverter{"small": &fakeConv{task: "small"}}
	h.im.add("IMG_1.jpg", photo(1))
	if _, err := h.run(); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{TagConvert, TagConvert + "/small", TagKeep} {
		if h.im.tags[v] == "" {
			t.Errorf("%s was not created", v)
		}
	}
	for _, v := range []string{TagFailed, TagReview, TagPrefix + "redo", TagPrefix + "reconvert"} {
		if h.im.tags[v] != "" {
			t.Errorf("%s was created", v)
		}
	}
	// A second run creates nothing more.
	h.im.tagCalls = nil
	if _, err := h.run(); err != nil || len(h.im.tagCalls) != 0 {
		t.Errorf("second run: %v, %v", err, h.im.tagCalls)
	}
}

// Keep protects an asset for as long as it has the tag: nothing is written
// to it, and removing the tag ends it.
func TestKeepProtectsWhileTagged(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagKeep)
	h.im.tag(a, TagConvert)
	sum, err := h.run()
	if err != nil || sum.Replaced != 0 || sum.Skipped != 1 || a.IsTrashed || h.marker(a.ID) != nil {
		t.Fatalf("%+v, %v, marker %+v", sum, err, h.marker(a.ID))
	}
	if !h.im.hasTag(a, TagKeep) || h.im.hasTag(a, TagConvert) {
		t.Errorf("tags = %v: keep stays, convert is settled", a.TagIDs)
	}
	// Keep removed, convert again: converted.
	if err := h.im.UntagAssets(context.Background(), h.im.tags[TagKeep], []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	h.im.tag(a, TagConvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 1 || !a.IsTrashed {
		t.Fatalf("after removing keep: %+v, %v", sum, err)
	}
}

// Keep on a conversion protects it from being converted again, and leaves
// its record as it is.
func TestKeepOnAConversion(t *testing.T) {
	h := newHarness(t)
	_, conv := convertOne(t, h, photo(1))
	h.im.convertAll = false
	h.conv.jxlToo = true
	before := string(conv.metadata[entity.MarkerKey])
	h.im.tag(conv, TagKeep)
	h.im.tag(conv, TagConvert)
	sum, err := h.run()
	if err != nil || sum.Replaced != 0 || conv.IsTrashed || string(conv.metadata[entity.MarkerKey]) != before || !h.im.hasTag(conv, TagKeep) {
		t.Fatalf("%+v, %v", sum, err)
	}
}

// The unfinished copy of an interrupted replacement is settled first, also
// when it carries a tag; the tag waits for a later run.
func TestTaggedUnfinishedCopyIsSettledFirst(t *testing.T) {
	h := tagged(t)
	old := h.im.add("IMG_1.jpg", photo(1))
	old.IsTrashed = true // the interrupted run got as far as the trash
	n := pendingCopy(h, old)
	h.im.tag(n, TagConvert)
	sum, err := h.run()
	if err != nil || sum.Finalized != 1 || sum.Replaced != 0 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if m := h.marker(n.ID); m == nil || m.State != "" {
		t.Errorf("marker after finishing = %+v", m)
	}
	if !h.im.hasTag(n, TagConvert) {
		t.Error("the tag was removed before it was acted on")
	}
}

// Tagging a conversion convert converts it again, from the converted file,
// with the record of every version kept and the first original still the
// one phones have.
func TestConvertAgainAppendsAVersion(t *testing.T) {
	h := newHarness(t)
	data := photo(1)
	old, conv := convertOne(t, h, data)
	h.im.convertAll = false
	h.conv.jxlToo = true
	first := h.marker(conv.ID)

	h.im.tag(conv, TagConvert)
	sum, err := h.run()
	if err != nil || sum.Replaced != 1 || !conv.IsTrashed {
		t.Fatalf("convert again: %+v, %v", sum, err)
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
	if m.State != "" || m.Replaces != conv.ID || h.im.hasTag(again, TagConvert) {
		t.Errorf("latest version: %+v, tags %v", m, again.TagIDs)
	}
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
	h.im.tag(again, TagConvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 1 {
		t.Fatalf("third version: %+v, %v", sum, err)
	}
	if m := h.marker(h.convertedFrom(again).ID); m == nil || len(m.Earlier) != 2 || m.SHA1 != old.Checksum {
		t.Errorf("third version record: %+v", m)
	}
}

// When converting again gives nothing better, the asset and its record stay
// exactly as they are; no "kept" mark replaces its history.
func TestConvertAgainWithoutGainChangesNothing(t *testing.T) {
	h := newHarness(t)
	_, conv := convertOne(t, h, photo(1))
	h.im.convertAll = false
	h.conv.jxlToo, h.conv.noGain = true, true
	before := string(conv.metadata[entity.MarkerKey])
	h.im.tag(conv, TagConvert)
	sum, err := h.run()
	if err != nil || sum.Replaced != 0 || sum.Kept != 0 || sum.Skipped != 1 || conv.IsTrashed {
		t.Fatalf("%+v, %v", sum, err)
	}
	if string(conv.metadata[entity.MarkerKey]) != before || h.im.hasTag(conv, TagConvert) {
		t.Errorf("the record changed or the tag stayed:\n%s", conv.metadata[entity.MarkerKey])
	}
}

// An asset kept because converting did not help is tried again when it is
// tagged again.
func TestConvertRetriesWhatWasKept(t *testing.T) {
	h := tagged(t)
	a := h.im.add("IMG_1.jpg", photo(1))
	h.im.tag(a, TagConvert)
	h.conv.noGain = true
	if sum, err := h.run(); err != nil || sum.Kept != 1 || h.marker(a.ID) == nil || !h.marker(a.ID).Kept {
		t.Fatalf("first try: %+v, %v", sum, err)
	}
	h.conv.noGain = false
	h.im.tag(a, TagConvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 1 || !a.IsTrashed {
		t.Fatalf("second try: %+v, %v", sum, err)
	}
	if m := h.marker(h.convertedFrom(a).ID); m == nil || m.Kept || m.SHA1 != a.Checksum || len(m.Earlier) != 0 {
		t.Errorf("record = %+v", m)
	}
}

// A record from an earlier version, which does not say what the original
// was, is not built on: phones would lose the photo.
func TestConvertAgainNeedsAReadableRecord(t *testing.T) {
	h := tagged(t)
	h.conv.jxlToo = true
	a := h.im.add("IMG_1.jxl", photo(1))
	old := `{"v":1,"original":{"sha1":"aa","size":1},"conversion":{"task":{"name":"jpeg-xl"}}}`
	a.metadata[entity.MarkerKey] = json.RawMessage(old)
	h.im.tag(a, TagConvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 0 || sum.Skipped != 1 || a.IsTrashed || string(a.metadata[entity.MarkerKey]) != old {
		t.Fatalf("%+v, %v", sum, err)
	}
}

// convert/<profile> converts with that profile, also when the asset is
// tagged convert as well; tagged for two profiles at once, an asset fails
// with a message saying which.
func TestConvertWithAProfile(t *testing.T) {
	h := tagged(t)
	small := &fakeConv{task: "small"}
	h.cfg.Profiles = map[string]port.CheckingConverter{"small": small}
	a := h.im.add("IMG_1.jpg", photo(1))
	b := h.im.add("IMG_2.jpg", photo(2))
	h.im.tag(a, TagConvert+"/small")
	h.im.tag(b, TagConvert)
	if sum, err := h.run(); err != nil || sum.Replaced != 2 {
		t.Fatalf("%+v, %v", sum, err)
	}
	if m := h.marker(h.convertedFrom(a).ID); m == nil || m.Task != "small" {
		t.Errorf("a was not converted with the profile: %+v", m)
	}
	if m := h.marker(h.convertedFrom(b).ID); m == nil || m.Task != "jpeg-xl" {
		t.Errorf("b was not converted with the tasks file: %+v", m)
	}
	if h.im.hasTag(a, TagConvert+"/small") {
		t.Error("the profile's tag was not settled")
	}

	// convert and convert/small together: the profile, being more specific,
	// wins.
	d := h.im.add("IMG_4.jpg", photo(4))
	h.im.tag(d, TagConvert)
	h.im.tag(d, TagConvert+"/small")
	if sum, err := h.run(); err != nil || sum.Replaced != 1 {
		t.Fatalf("convert and a profile: %+v, %v", sum, err)
	}
	if m := h.marker(h.convertedFrom(d).ID); m == nil || m.Task != "small" {
		t.Errorf("convert and a profile: converted with %+v", m)
	}

	// Two profiles: which one is meant cannot be known.
	h.cfg.Profiles["other"] = &fakeConv{task: "other"}
	c := h.im.add("IMG_3.jpg", photo(3))
	h.im.tag(c, TagConvert+"/other")
	h.im.tag(c, TagConvert+"/small")
	sum, err := h.run()
	if err != nil || sum.Failed != 1 || c.IsTrashed || !h.im.hasTag(c, TagFailed) {
		t.Fatalf("two profiles: %+v, %v, tags %v", sum, err, c.TagIDs)
	}
	found := false
	for _, e := range h.entries() {
		found = found || e.Event == "failed" && e.Asset == c.ID && strings.Contains(e.Detail, TagConvert+"/small") && strings.Contains(e.Detail, TagConvert+"/other")
	}
	if !found {
		t.Error("the failure does not name the tags")
	}
}
