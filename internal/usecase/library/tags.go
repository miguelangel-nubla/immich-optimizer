package library

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// Library mode is driven by tags in Immich. The person (or one of Immich's
// own workflows) tags assets; library mode converts what is tagged and
// removes the tag. The optimizer sets the status tags itself.
const (
	// TagPrefix is the parent of every tag the optimizer uses.
	TagPrefix = "immich-optimizer/"
	// TagConvert asks for the asset to be converted with the tasks file;
	// TagConvert + "/" + a profile's name, with that profile. An asset
	// that is already converted is converted again, from its current file.
	TagConvert = TagPrefix + "convert"
	// TagKeep protects an asset: while it has this tag, it is not
	// converted, whatever else it is tagged. Removing the tag ends it.
	TagKeep = TagPrefix + "keep"
	// TagFailed is set on an asset whose conversion failed. Its convert
	// tag stays, and it is not tried again while it has this tag: removing
	// it is how a person asks for another try. One change each way, since
	// Immich can lose one of two quick tag changes to the same asset.
	TagFailed = TagPrefix + "failed"
	// TagReview is set on a second copy that was left alone because it has
	// something of its own: the person merges the two.
	TagReview = TagPrefix + "review"
)

// ConvertTag returns the tag that asks for a conversion with profile ("" is
// the tasks file).
func ConvertTag(profile string) string {
	if profile == "" {
		return TagConvert
	}
	return TagConvert + "/" + profile
}

// runTags is what one pass knows about the optimizer's tags.
type runTags struct {
	// ids maps every existing optimizer tag's value to its id.
	ids map[string]string
	// own is the set of those ids: they are never carried over or compared.
	own map[string]bool
	// convert maps each asset tagged to be converted to the profile it
	// asks for ("" is the tasks file). conflict holds the assets tagged
	// for more than one, with those profiles.
	convert  map[string]string
	conflict map[string][]string
	// convertTagged maps each convert tag's value to the assets that have it.
	convertTagged map[string]map[string]bool
	// keep are the assets tagged keep, failed those tagged failed.
	keep, failed map[string]bool
}

// commandTags are the tags a person sets: created when library mode starts,
// so that they can be found in Immich before they were ever used. The
// status tags appear when they are first set.
func (s *Service) commandTags() []string {
	tags := []string{TagConvert}
	for _, p := range s.profileNames() {
		tags = append(tags, ConvertTag(p))
	}
	return append(tags, TagKeep)
}

// profileNames returns the named profiles, sorted.
func (s *Service) profileNames() []string {
	names := make([]string, 0, len(s.cfg.Profiles))
	for n := range s.cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// converter returns the converter of a profile ("" is the tasks file).
func (s *Service) converter(profile string) port.CheckingConverter {
	if c := s.cfg.Profiles[profile]; profile != "" && c != nil {
		return c
	}
	return s.conv
}

// loadTags makes sure the command tags exist and reads which assets have
// them.
func (s *Service) loadTags(ctx context.Context) (*runTags, error) {
	all, err := s.base.Tags(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tags: %w", err)
	}
	var missing []string
	for _, v := range s.commandTags() {
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
		for _, v := range missing {
			if err := s.record(Entry{Event: "tag_created", Detail: v}); err != nil {
				return nil, err
			}
		}
	}
	rt := &runTags{ids: map[string]string{}, own: map[string]bool{}, convert: map[string]string{},
		conflict: map[string][]string{}, convertTagged: map[string]map[string]bool{}}
	for v, id := range all {
		if strings.HasPrefix(v, TagPrefix) && len(v) > len(TagPrefix) {
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
	counts := []string{}
	// The profiles first: Immich's search for a tag also finds the assets
	// of its child tags, so the plain convert tag finds every one of them
	// too. A profile is more specific and wins; two profiles conflict.
	for _, profile := range s.profileNames() {
		v := ConvertTag(profile)
		set, err := tagged(v)
		if err != nil {
			return nil, err
		}
		rt.convertTagged[v] = set
		for id := range set {
			if prev, ok := rt.convert[id]; ok {
				if len(rt.conflict[id]) == 0 {
					rt.conflict[id] = []string{prev}
				}
				rt.conflict[id] = append(rt.conflict[id], profile)
				continue
			}
			rt.convert[id] = profile
		}
		if len(set) > 0 {
			counts = append(counts, fmt.Sprintf("%s: %d", v, len(set)))
		}
	}
	plain, err := tagged(TagConvert)
	if err != nil {
		return nil, err
	}
	rt.convertTagged[TagConvert] = plain
	n := 0
	for id := range plain {
		if _, ok := rt.convert[id]; !ok {
			rt.convert[id] = ""
			n++
		}
	}
	if n > 0 {
		counts = append(counts, fmt.Sprintf("%s: %d", TagConvert, n))
	}
	if rt.keep, err = tagged(TagKeep); err != nil {
		return nil, err
	}
	if id := rt.ids[TagFailed]; id != "" {
		if rt.failed, err = tagged(TagFailed); err != nil {
			return nil, err
		}
	}
	if len(rt.keep) > 0 {
		counts = append(counts, fmt.Sprintf("%s: %d", TagKeep, len(rt.keep)))
	}
	if len(counts) == 0 {
		counts = append(counts, "nothing")
	}
	s.infof("Library: tagged %s", strings.Join(counts, ", "))
	return rt, nil
}

// knownTags are every tag the optimizer uses, the status tags included.
func (s *Service) knownTags() map[string]bool {
	known := map[string]bool{TagFailed: true, TagReview: true}
	for _, v := range s.commandTags() {
		known[v] = true
	}
	return known
}

// tidyTags deletes the tags under TagPrefix that the optimizer does not use
// (left by an earlier version, or by a profile that was removed), so that
// Immich's tag picker only offers what works. A tag that assets still carry
// is left alone, with a warning: deleting it would silently drop what was
// asked of them.
func (s *Service) tidyTags(ctx context.Context) error {
	all, err := s.base.Tags(ctx)
	if err != nil {
		return fmt.Errorf("listing tags: %w", err)
	}
	known := s.knownTags()
	var unknown []string
	for v := range all {
		if strings.HasPrefix(v, TagPrefix) && !known[v] {
			unknown = append(unknown, v)
		}
	}
	sort.Strings(unknown)
	for _, v := range unknown {
		ids, err := s.base.TaggedAssetIDs(ctx, all[v])
		if err != nil {
			return fmt.Errorf("finding the assets tagged %s: %w", v, err)
		}
		if len(ids) > 0 {
			if s.log != nil {
				s.log.Warnf("Library: the tag %s is not one the optimizer uses, but %d asset(s) have it: it is left alone. Remove it from them, or add back the profile it names", v, len(ids))
			}
			continue
		}
		if err := s.base.DeleteTag(ctx, all[v]); err != nil {
			return fmt.Errorf("deleting the unused tag %s: %w", v, err)
		}
		s.infof("Library: deleted the tag %s, which the optimizer does not use", v)
		if err := s.record(Entry{Event: "tag_deleted", Detail: v}); err != nil {
			return err
		}
	}
	return nil
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

// dropOwnTags removes the optimizer's tags from an asset this run created.
func (s *Service) dropOwnTags(ctx context.Context, id string) error {
	a, err := s.base.GetAsset(ctx, id)
	if err != nil {
		return fmt.Errorf("reading the new asset's tags: %w", err)
	}
	for _, t := range a.TagIDs {
		if s.own[t] {
			if _, err := s.base.UntagAssets(ctx, t, []string{id}); err != nil {
				return fmt.Errorf("removing the optimizer's tag from the new asset: %w", err)
			}
			if err := s.record(Entry{Event: "tag_removed", Asset: id, Detail: s.tagValue(t) + " (copied over by Immich)"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// tagValue returns the value of one of the optimizer's tags, from its id.
func (s *Service) tagValue(id string) string {
	for v, tid := range s.tagValues {
		if tid == id {
			return v
		}
	}
	return id
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

// settleTags removes the convert tags from the assets a pass acted on and
// sets the status tags. The keep tag is never removed: it is the person's.
// It runs at the end of a pass that was not interrupted; a pass that stops
// earlier leaves the tags, and the next one acts on them again.
func (s *Service) settleTags(ctx context.Context, rt *runTags, items []*Item) error {
	status := make(map[string]*Item, len(items))
	for _, it := range items {
		status[it.ID] = it
	}
	// The failed tag holds back a convert tag: it is of no use on an asset
	// nobody tagged (a second copy, an unfinished copy).
	tagged := map[string]bool{}
	for _, ids := range rt.convertTagged {
		for id := range ids {
			tagged[id] = true
		}
	}
	var failed, review []string
	for _, it := range items {
		switch {
		case it.Status == statusFailed:
			if tagged[it.ID] {
				failed = append(failed, it.ID)
			}
		case it.review:
			review = append(review, it.ID)
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
			if err == nil && created[tag] == "" {
				err = errors.New("no id came back from Immich")
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("creating the tag %s: %w", tag, err))
				return
			}
			id = created[tag]
			rt.ids[tag], rt.own[id] = id, true
			if err := s.record(Entry{Event: "tag_created", Detail: tag}); err != nil {
				errs = append(errs, err)
				return
			}
		}
		var changed []string
		var err error
		event := "tag_removed"
		if add {
			changed, err = s.base.TagAssets(ctx, id, ids)
			event = "tag_added"
		} else {
			changed, err = s.base.UntagAssets(ctx, id, ids)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("updating the tag %s on %d asset(s): %w", tag, len(ids), err))
			return
		}
		// Only what changed: Immich's search for a tag also finds the
		// assets of its child tags, which do not have the tag itself.
		for _, a := range changed {
			if err := s.record(Entry{Event: event, Asset: a, Detail: tag}); err != nil {
				errs = append(errs, err)
				return
			}
		}
	}
	tags := make([]string, 0, len(rt.convertTagged))
	for v := range rt.convertTagged {
		tags = append(tags, v)
	}
	sort.Strings(tags)
	for _, v := range tags {
		var settled []string
		for id := range rt.convertTagged[v] {
			// A failed asset keeps its convert tag: see TagFailed.
			if it := status[id]; it != nil && !it.retry && it.Status != statusFailed {
				settled = append(settled, id)
				// Immich can apply the tags of the sidecar it copied to the
				// new asset late, after the replacement removed them: the
				// new asset would then be converted again. Only the assets
				// that have the tag are changed (and logged).
				if it.Status == statusReplaced && it.NewID != "" {
					settled = append(settled, it.NewID)
				}
			}
		}
		change(false, v, settled)
	}
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
