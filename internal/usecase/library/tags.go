package library

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// Library mode is driven by tags in Immich. The person (or one of Immich's
// own workflows) tags assets; each run carries the command out and removes
// the tag. The optimizer sets the status tags itself.
const (
	// TagPrefix is the parent of every tag the optimizer uses.
	TagPrefix = "immich-optimizer/"
	// TagConvert asks for the asset to be converted.
	TagConvert = TagPrefix + "convert"
	// TagDone marks the asset as handled: it is never converted.
	TagDone = TagPrefix + "done"
	// TagRedo removes a "done" (or "kept") mark, so that the asset can be
	// converted again.
	TagRedo = TagPrefix + "redo"
	// TagFailed is set on an asset whose conversion failed.
	TagFailed = TagPrefix + "failed"
	// TagReview is set on a second copy that was left alone because it has
	// something of its own: the person merges the two.
	TagReview = TagPrefix + "review"
)

// commandTags are created when library mode starts, so that they can be
// found in Immich before they were ever used. The status tags appear when
// they are first set.
var commandTags = []string{TagConvert, TagDone, TagRedo}

// errPendingCopy says a tagged asset is the unfinished copy of an
// interrupted replacement. Settling that comes first, whatever the tag says;
// the tag waits for a later run.
var errPendingCopy = errors.New("the unfinished copy of an interrupted replacement")

// DoneNote is the note of the marker written for TagDone.
const DoneNote = "marked as done in Immich"

// runTags is what one run knows about the optimizer's tags.
type runTags struct {
	// ids maps every existing optimizer tag's value to its id.
	ids map[string]string
	// own is the set of those ids: they are never carried over or compared.
	own map[string]bool
	// convert, done and redo are the assets that carry each command.
	convert, done, redo map[string]bool
}

// loadTags makes sure the command tags exist and reads which assets have
// them.
func (s *Service) loadTags(ctx context.Context) (*runTags, error) {
	all, err := s.base.Tags(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tags: %w", err)
	}
	var missing []string
	for _, v := range commandTags {
		if all[v] == "" {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		created, err := s.base.UpsertTags(ctx, missing)
		if err != nil {
			return nil, fmt.Errorf("creating the tags %v: %w", missing, err)
		}
		for v, id := range created {
			all[v] = id
		}
		for _, v := range missing {
			if all[v] == "" {
				return nil, fmt.Errorf("creating the tag %s: Immich did not return it", v)
			}
		}
		s.infof("Library: created the tags %v in Immich", missing)
	}
	rt := &runTags{ids: map[string]string{}, own: map[string]bool{}}
	for v, id := range all {
		if len(v) > len(TagPrefix) && v[:len(TagPrefix)] == TagPrefix {
			rt.ids[v], rt.own[id] = id, true
		}
	}
	tagged := func(v string) (map[string]bool, error) {
		ids, err := s.base.TaggedAssetIDs(ctx, rt.ids[v])
		if err != nil {
			return nil, fmt.Errorf("finding the assets tagged %s: %w", v, err)
		}
		set := make(map[string]bool, len(ids))
		for _, id := range ids {
			set[id] = true
		}
		return set, nil
	}
	if rt.convert, err = tagged(TagConvert); err != nil {
		return nil, err
	}
	if rt.done, err = tagged(TagDone); err != nil {
		return nil, err
	}
	if rt.redo, err = tagged(TagRedo); err != nil {
		return nil, err
	}
	s.infof("Library: tagged %s: %d, %s: %d, %s: %d", TagConvert, len(rt.convert), TagDone, len(rt.done), TagRedo, len(rt.redo))
	return rt, nil
}

// withoutOwnTags hides the optimizer's tags from everything that reads an
// asset: they are commands and status for this asset only, and must be
// neither carried over to a converted copy nor counted as a difference.
type withoutOwnTags struct {
	port.LibraryAPI
	own map[string]bool
}

func (w withoutOwnTags) strip(ids []string) []string {
	if len(ids) == 0 {
		return ids
	}
	out := ids[:0:0]
	for _, id := range ids {
		if !w.own[id] {
			out = append(out, id)
		}
	}
	return out
}

func (w withoutOwnTags) GetAsset(ctx context.Context, id string) (*entity.LibraryAsset, error) {
	a, err := w.LibraryAPI.GetAsset(ctx, id)
	if a != nil {
		a.TagIDs = w.strip(a.TagIDs)
	}
	return a, err
}

func (w withoutOwnTags) ListAssets(ctx context.Context, page int) ([]entity.LibraryAsset, int, error) {
	assets, next, err := w.LibraryAPI.ListAssets(ctx, page)
	for i := range assets {
		assets[i].TagIDs = w.strip(assets[i].TagIDs)
	}
	return assets, next, err
}

// dropOwnTags removes the optimizer's tags from an asset this run created.
func (s *Service) dropOwnTags(ctx context.Context, id string) error {
	a, err := s.base.GetAsset(ctx, id)
	if err != nil {
		return fmt.Errorf("reading the new asset's tags: %w", err)
	}
	for _, t := range a.TagIDs {
		if s.own[t] {
			if err := s.base.UntagAssets(ctx, t, []string{id}); err != nil {
				return fmt.Errorf("removing the optimizer's tag from the new asset: %w", err)
			}
		}
	}
	return nil
}

// basicChecks are the reasons to leave an asset alone that the listing
// already shows.
func basicChecks(me string, listed entity.LibraryAsset) error {
	switch {
	case listed.OwnerID != me:
		return skip("it belongs to another user")
	case listed.LibraryID != "":
		return skip("it is in an external library")
	case listed.IsTrashed:
		return later("it is in the trash")
	case listed.IsOffline:
		return later("its file is offline")
	case listed.Visibility != "timeline" && listed.Visibility != "archive":
		return skip("its visibility is %q", listed.Visibility)
	}
	return nil
}

// markDone writes the "done" marker on an asset tagged TagDone. The file is
// not touched. An asset the optimizer already handled keeps its marker.
func (s *Service) markDone(ctx context.Context, me string, listed entity.LibraryAsset, t *tally) error {
	if err := basicChecks(me, listed); err != nil {
		return err
	}
	items, err := s.api.GetMetadata(ctx, listed.ID)
	if err != nil {
		return fmt.Errorf("reading asset metadata: %w", err)
	}
	if m := markerOf(items); m != nil {
		if m.State == entity.MarkerStatePending {
			return errPendingCopy
		}
		return &errSkip{reason: "it was already handled by the optimizer", converted: m}
	}
	t.item.Size = listed.Size
	if err := s.writeKept(ctx, &listed, DoneNote); err != nil {
		return err
	}
	t.MarkedDone++
	t.item.Status, t.item.Reason = statusMarkedDone, "tagged "+TagDone
	s.infof("Library: marked %s (%s) as done", listed.OriginalFileName, listed.ID)
	return s.record(Entry{Event: "marked_done", Asset: listed.ID, File: listed.OriginalFileName, OriginalBytes: listed.Size})
}

// redo removes the marker of an asset tagged TagRedo, when that marker only
// says the asset was left as it is ("done", or kept because converting it
// did not help). The marker of a conversion stays: it is what tells phones
// and later runs about the original.
//
// It returns nil when the asset may now be considered like any unmarked
// asset, and an error (usually a skip) otherwise.
func (s *Service) redo(ctx context.Context, me string, listed entity.LibraryAsset, t *tally) error {
	if err := basicChecks(me, listed); err != nil {
		return err
	}
	items, err := s.api.GetMetadata(ctx, listed.ID)
	if err != nil {
		return fmt.Errorf("reading asset metadata: %w", err)
	}
	m := markerOf(items)
	switch {
	case m == nil:
		return nil
	case m.State == entity.MarkerStatePending:
		return errPendingCopy
	case !m.Kept:
		return &errSkip{reason: "tagged " + TagRedo + ", but it is a conversion: its marker stays", converted: m}
	}
	if err := s.api.DeleteMetadata(ctx, listed.ID, entity.MarkerKey); err != nil {
		return fmt.Errorf("removing its marker: %w", err)
	}
	t.Forgotten++
	s.infof("Library: removed the marker of %s (%s), as tagged %s", listed.OriginalFileName, listed.ID, TagRedo)
	return s.record(Entry{Event: "marker_removed", Asset: listed.ID, File: listed.OriginalFileName, Detail: m.Note})
}

// settleTags removes the command tags from the assets the run acted on and
// sets the status tags. It runs at the end of a real run that was not
// interrupted; a run that stops earlier leaves the tags, and the next run
// acts on them again, which is harmless: what was done is found done.
func (s *Service) settleTags(ctx context.Context, rt *runTags, items []*Item) error {
	status := make(map[string]*Item, len(items))
	for _, it := range items {
		status[it.ID] = it
	}
	var untagConvert, untagDone, untagRedo, untagFailed, failed, review []string
	for id := range rt.convert {
		if it := status[id]; it != nil && !it.retry {
			untagConvert = append(untagConvert, id)
		}
	}
	for id := range rt.done {
		if it := status[id]; it != nil && !it.retry {
			untagDone = append(untagDone, id)
			// Done wins over convert: both commands are settled.
			if rt.convert[id] {
				untagConvert = append(untagConvert, id)
			}
		}
	}
	for id := range rt.redo {
		if it := status[id]; it != nil && !it.retry {
			untagRedo = append(untagRedo, id)
		}
	}
	for _, it := range items {
		switch {
		case it.Status == statusFailed:
			failed = append(failed, it.ID)
		case it.review:
			review = append(review, it.ID)
		case it.Status == statusKept || it.Status == statusMarkedDone:
			untagFailed = append(untagFailed, it.ID)
		}
	}

	var errs []error
	change := func(add bool, tag string, ids []string) {
		if len(ids) == 0 {
			return
		}
		sort.Strings(ids)
		ids = dedupe(ids)
		id := rt.ids[tag]
		if id == "" {
			if !add {
				return // the tag does not exist, so no asset has it
			}
			created, err := s.base.UpsertTags(ctx, []string{tag})
			if err != nil || created[tag] == "" {
				errs = append(errs, fmt.Errorf("creating the tag %s: %v", tag, err))
				return
			}
			id = created[tag]
			rt.ids[tag], rt.own[id] = id, true
		}
		var err error
		if add {
			err = s.base.TagAssets(ctx, id, ids)
		} else {
			err = s.base.UntagAssets(ctx, id, ids)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("updating the tag %s on %d asset(s): %w", tag, len(ids), err))
		}
	}
	change(false, TagConvert, untagConvert)
	change(false, TagDone, untagDone)
	change(false, TagRedo, untagRedo)
	change(false, TagFailed, untagFailed)
	change(true, TagFailed, failed)
	change(true, TagReview, review)
	return errors.Join(errs...)
}

func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}
