// Package library converts the assets that already exist in an Immich
// library. Immich has no "replace this file" call, so each asset is replaced
// in a fixed order that never leaves the user without their photo:
//
//  1. download the original and check it against Immich's checksum
//  2. convert it, and check that the result looks like a valid photo or video
//  3. upload the converted file as a new asset (marked "pending")
//  4. carry everything over from the original to the new asset
//  5. verify that the new asset matches the original in every compared way
//  6. only then move the original to Immich's trash
//  7. clear the "pending" mark
//
// A failure at any point before step 6 deletes the new asset again and leaves
// the original untouched. Every step is written to a persistent log. The only
// state is the marker in Immich's asset metadata, which is also what lets an
// interrupted run be finished or rolled back by the next one.
package library

import (
	"context"
	"crypto/sha1" // #nosec G505 -- content identity matching Immich, not security
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

type Config struct {
	// Label names the API key in the log until its user is known, when
	// there are several ("key 2 of 3"). Once known, the log names the
	// user by id instead, which does not change with the key.
	Label string
	// ReadyTimeout is how long to wait for Immich to finish reading a newly
	// uploaded asset before giving up on it.
	ReadyTimeout time.Duration
	// PollInterval is the delay between readiness checks.
	PollInterval time.Duration
	// Version is the optimizer version recorded in markers, and
	// ConfigSHA256 the fingerprint of the tasks file's folder.
	Version      string
	ConfigSHA256 string
	// Concurrency is how many assets are worked on at the same time.
	Concurrency int
	// IsMotionPhoto reports whether a downloaded original is a photo with an
	// embedded video. Optional.
	IsMotionPhoto func(path string) bool
}

// Entry is an audit log entry.
type Entry = audit.Entry

// tally is what working on one asset produced: its share of the summary and
// its row in the report.
type tally struct {
	Summary
	item Item
}

// Summary counts what a run did.
type Summary struct {
	Seen       int `json:"seen"`
	Replaced   int `json:"replaced"`
	Kept       int `json:"kept"`
	Skipped    int `json:"skipped"`
	Failed     int `json:"failed"`
	Finalized  int `json:"finalized,omitempty"`
	RolledBack int `json:"rolled_back,omitempty"`
	// SecondCopies are originals that were uploaded again after their
	// conversion and were moved to the trash.
	SecondCopies int `json:"second_copies,omitempty"`
	// MarkedKeep are assets tagged "done" that got their marker, and
	// Forgotten those tagged "redo" whose marker was removed.
	MarkedKeep  int   `json:"marked_keep,omitempty"`
	Forgotten   int   `json:"forgotten,omitempty"`
	BytesBefore int64 `json:"bytes_before"`
	BytesAfter  int64 `json:"bytes_after"`
}

type Service struct {
	cfg Config
	// api is base during a run with the optimizer's own tags hidden.
	api  port.LibraryAPI
	base port.LibraryAPI
	// own are the ids of the optimizer's tags, during a run.
	own map[string]bool
	// rootLog is log without the label or user prefix.
	rootLog *logger.Logger
	// me is the API key's user, once known.
	me string
	// idx is what the audit log knows, while Serve runs.
	idx     *logIndex
	conv    port.CheckingConverter
	log     *logger.Logger
	journal *audit.Log

	// memories says, for this run, which memories each asset is in.
	memories map[string][]string

	// stacks serialises replacements inside one stack: two members worked on
	// at once would fight over the stack's cover.
	stacksMu sync.Mutex
	stacks   map[string]*sync.Mutex
}

func (s *Service) lockStack(id string) func() {
	if id == "" {
		return func() {}
	}
	s.stacksMu.Lock()
	if s.stacks == nil {
		s.stacks = map[string]*sync.Mutex{}
	}
	m := s.stacks[id]
	if m == nil {
		m = &sync.Mutex{}
		s.stacks[id] = m
	}
	s.stacksMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (sum *Summary) add(o Summary) {
	sum.Seen += o.Seen
	sum.Replaced += o.Replaced
	sum.Kept += o.Kept
	sum.Skipped += o.Skipped
	sum.Failed += o.Failed
	sum.Finalized += o.Finalized
	sum.RolledBack += o.RolledBack
	sum.SecondCopies += o.SecondCopies
	sum.MarkedKeep += o.MarkedKeep
	sum.Forgotten += o.Forgotten
	sum.BytesBefore += o.BytesBefore
	sum.BytesAfter += o.BytesAfter
}

func New(cfg Config, api port.LibraryAPI, conv port.CheckingConverter, journal *audit.Log, log *logger.Logger) *Service {
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 10 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	s := &Service{cfg: cfg, api: api, base: api, conv: conv, log: log, rootLog: log, journal: journal}
	if cfg.Label != "" {
		s.log = log.WithPrefix("[" + cfg.Label + "] ")
	}
	return s
}

// errSkip is returned for assets library mode leaves alone on purpose.
type errSkip struct {
	reason string
	// converted is the marker of an asset that is skipped because it is a
	// finished conversion.
	converted *entity.Marker
	// retry means the reason may go away by itself: a tag asking for the
	// asset stays, so that a later run tries again.
	retry bool
	// review means the person should look at the asset.
	review bool
}

func (e *errSkip) Error() string { return e.reason }

func skip(format string, args ...any) error { return &errSkip{reason: fmt.Sprintf(format, args...)} }

// later is a skip whose reason may go away by itself.
func later(format string, args ...any) error {
	return &errSkip{reason: fmt.Sprintf(format, args...), retry: true}
}

// errNotSecondCopy says an asset whose file is a converted original is not
// a second copy after all: the conversion is not this user's, or is gone.
var errNotSecondCopy = errors.New("not a second copy")

// errChanged means the original was modified (by the user, or by Immich
// still analysing it) while its replacement was under way. The copy is
// removed and the asset is left for a later run: nothing went wrong.
type errChanged struct{ what string }

func (e *errChanged) Error() string { return e.what }

// errFatal stops the whole run: continuing could act without a record.
type errFatal struct{ err error }

func (e *errFatal) Error() string { return e.err.Error() }
func (e *errFatal) Unwrap() error { return e.err }

func (s *Service) infof(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

// record writes a log entry; a log that cannot be written stops the run.
func (s *Service) record(e Entry) error {
	e.Source = string(entity.SourceLibrary)
	if e.User == "" {
		e.User = s.me
	}
	if err := s.journal.Write(e); err != nil {
		return &errFatal{err}
	}
	return nil
}

// Run makes one pass over the library.
//
// It is a full read: every asset's metadata is read, which finds whatever
// earlier runs left unfinished and every converted original. Library mode
// does this when the audit log has nothing to go by; otherwise it only looks
// at what the tags, the events and the log point at (see Serve).
func (s *Service) Run(ctx context.Context) (Summary, error) {
	u, err := s.base.CurrentUser(ctx)
	if err != nil {
		return Summary{}, fmt.Errorf("identifying the API key's user: %w", err)
	}
	me := u.ID
	rt, err := s.loadTags(ctx)
	if err != nil {
		return Summary{}, err
	}
	// List everything first: replacing assets while paging through them
	// would shift the pages.
	var assets []entity.LibraryAsset
	for page := 1; page != 0; {
		items, next, err := s.base.ListAssets(ctx, page)
		if err != nil {
			return Summary{}, fmt.Errorf("listing assets: %w", err)
		}
		assets = append(assets, items...)
		page = next
	}
	s.infof("Library: full read of the library: %d asset(s)", len(assets))
	sum, err := s.pass(ctx, me, rt, assets)
	if err == nil {
		err = s.record(Entry{Event: eventFullRead})
	}
	return sum, err
}

// pass works on the given assets: it acts on their tags, settles unfinished
// replacements, trashes second copies and converts what is tagged.
func (s *Service) pass(ctx context.Context, me string, rt *runTags, assets []entity.LibraryAsset) (Summary, error) {
	var sum Summary
	s.me = me
	if err := s.record(Entry{Event: "run_started", Detail: "user " + me}); err != nil {
		return sum, err
	}
	// The optimizer's own tags are hidden from every read of an asset for
	// the rest of the pass.
	s.api, s.own = withoutOwnTags{s.base, rt.own}, rt.own
	defer func() { s.api, s.own = s.base, nil }()
	for i := range assets {
		assets[i].TagIDs = withoutOwnTags{own: rt.own}.strip(assets[i].TagIDs)
	}
	var err error

	// Memories ("on this day") list their assets by id, so a replaced
	// asset has to be put into the memories of its original. Read once: a
	// real run needs it for every replacement.
	if s.memories, err = s.api.MemoryIDs(ctx); err != nil {
		return sum, fmt.Errorf("listing memories: %w", err)
	}

	// outcome folds one asset's result into the summary. It returns a fatal
	// error when the run must stop.
	var mu sync.Mutex
	var fatalErr error
	var items []*Item
	outcome := func(a entity.LibraryAsset, t *tally, err error) {
		mu.Lock()
		defer mu.Unlock()
		sum.add(t.Summary)
		it := &t.item
		it.fill(a)
		defer func() {
			// An unfinished copy that was only looked at in a dry run, or
			// an interrupted asset, still gets a row.
			if it.Status == "" {
				it.Status = statusSkipped
			}
			items = append(items, it)
		}()
		var sk *errSkip
		var fatal *errFatal
		switch {
		case err == nil:
		case errors.As(err, &fatal):
			if fatalErr == nil {
				fatalErr = fatal.err
			}
		case ctx.Err() != nil:
			// Interrupted: whatever was in flight has been rolled back or
			// will be finished by the next run.
			it.retry = true
		case errors.As(err, &sk):
			sum.Skipped++
			it.Status, it.Reason = statusSkipped, sk.reason
			it.retry, it.review = it.retry || sk.retry, it.review || sk.review
			if s.log != nil {
				s.log.Debugf("Library: skipping %s (%s): %s", a.OriginalFileName, a.ID, sk.reason)
			}
		default:
			sum.Failed++
			it.Status, it.Reason = statusFailed, err.Error()
			if s.log != nil {
				s.log.Errorf("Library: %s (%s) failed: %v", a.OriginalFileName, a.ID, err)
			}
			if rerr := s.record(Entry{Event: "failed", Asset: a.ID, File: a.OriginalFileName, Detail: err.Error()}); rerr != nil && fatalErr == nil {
				var f *errFatal
				errors.As(rerr, &f)
				fatalErr = f.err
			}
		}
	}
	stopped := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return fatalErr != nil || ctx.Err() != nil
	}
	// 1. Look at every asset: most are skipped here, cheaply.
	type candidate struct {
		asset entity.LibraryAsset
		items []entity.MetadataItem
	}
	type pendingCopy struct {
		asset  entity.LibraryAsset
		marker *entity.Marker
	}
	var candidates []candidate
	var pendings []pendingCopy
	// convertedFrom maps the checksum of an original to the asset it was
	// converted into, from the markers read in this pass (the audit log
	// knows more, see below).
	// converted into, from the markers read in this pass. Nothing is stored:
	// it is built again on every run.
	convertedFrom := map[string]string{}
	s.forEach(len(assets), stopped, func(i int) {
		a := assets[i]
		var meta []entity.MetadataItem
		var m *entity.Marker
		var err error
		t := &tally{Summary: Summary{Seen: 1}}
		switch {
		case rt.keep[a.ID]:
			// "Done" wins over every other tag.
			err = s.markKeep(ctx, me, a, t)
			if err == nil {
				outcome(a, t, nil)
				return
			}
		case rt.redo[a.ID]:
			err = s.redo(ctx, me, a, t)
			if err == nil {
				meta, m, err = s.classify(ctx, me, a)
			}
		default:
			meta, m, err = s.classify(ctx, me, a)
		}
		if errors.Is(err, errPendingCopy) {
			meta, m, err = s.classify(ctx, me, a)
		}
		var sk *errSkip
		if errors.As(err, &sk) && sk.converted != nil && !sk.converted.Kept && sk.converted.SHA1 != "" {
			mu.Lock()
			convertedFrom[strings.ToLower(sk.converted.SHA1)] = a.ID
			mu.Unlock()
		}
		if err == nil {
			// Decided later, in step 2 or 3: only count it as seen here
			// (and a "redo" that freed it).
			mu.Lock()
			sum.add(t.Summary)
			if m != nil {
				pendings = append(pendings, pendingCopy{a, m})
			} else {
				candidates = append(candidates, candidate{a, meta})
			}
			mu.Unlock()
			return
		}
		outcome(a, t, err)
	})

	// 2. Unfinished replacements of an earlier run are settled first, one by
	// one, before any of their originals is touched again.
	for _, p := range pendings {
		if stopped() {
			break
		}
		t := &tally{}
		// A tag on the unfinished copy waits for a later run.
		t.item.retry = rt.keep[p.asset.ID] || rt.redo[p.asset.ID] || rt.convert[p.asset.ID]
		outcome(p.asset, t, s.resumePending(ctx, p.asset, p.marker, t))
	}

	// 3. An unconverted asset with the checksum of an original that is
	// already converted is a second copy of it: a phone uploads its photo
	// again once the original has left Immich's trash. It is not converted a
	// second time (Immich would refuse the identical result); it goes to the
	// trash.
	rest := candidates[:0]
	for _, c := range candidates {
		if stopped() {
			break
		}
		convertedID, second := convertedFrom[strings.ToLower(c.asset.Checksum)]
		if !second && s.idx != nil {
			convertedID, second = s.idx.convertedFrom(c.asset.Checksum)
			second = second && convertedID != c.asset.ID
		}
		if second && c.asset.Checksum != "" {
			t := &tally{}
			if err := s.secondCopy(ctx, c.asset, convertedID, t); !errors.Is(err, errNotSecondCopy) {
				outcome(c.asset, t, err)
				continue
			}
			// The conversion it pointed at is not this user's: an
			// ordinary candidate.
		}
		if !rt.convert[c.asset.ID] {
			outcome(c.asset, &tally{}, skip("it is not tagged %s", TagConvert))
			continue
		}
		rest = append(rest, c)
	}
	candidates = rest

	// 4. Convert, oldest first.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].asset.FileCreatedAt.Before(candidates[j].asset.FileCreatedAt)
	})
	s.forEach(len(candidates), stopped, func(i int) {
		c := candidates[i]
		t := &tally{}
		outcome(c.asset, t, s.work(ctx, c.asset, c.items, t))
	})
	// 5. The tags of what was acted on are settled.
	if fatalErr == nil && ctx.Err() == nil {
		if err := s.settleTags(ctx, rt, items); err != nil && s.log != nil {
			s.log.Warnf("Library: %v; the next run tries again", err)
		}
	}
	if fatalErr != nil {
		return sum, fatalErr
	}

	if s.idx != nil {
		s.idx.remember(convertedFrom)
	}
	if err := s.record(Entry{Event: "run_finished", Summary: &sum}); err != nil {
		return sum, err
	}
	s.infof("Library: pass finished: %d seen, %d replaced, %d kept, %d tagged keep, %d second copies trashed, %d skipped, %d failed (%d -> %d bytes)",
		sum.Seen, sum.Replaced, sum.Kept, sum.MarkedKeep, sum.SecondCopies, sum.Skipped, sum.Failed, sum.BytesBefore, sum.BytesAfter)
	return sum, ctx.Err()
}

// forEach runs fn(i) for i in [0,n) with the configured concurrency. stop is
// asked before each item is started; once it says yes, no more are started.
func (s *Service) forEach(n int, stop func() bool, fn func(i int)) {
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		if stop() {
			<-sem
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// markerOf returns the optimizer marker among items, if any.
func markerOf(items []entity.MetadataItem) *entity.Marker {
	for _, it := range items {
		if it.Key != entity.MarkerKey {
			continue
		}
		var m entity.Marker
		if err := json.Unmarshal(it.Value, &m); err != nil {
			// Unknown shape: treat it as "the optimizer has been here".
			return &entity.Marker{}
		}
		return &m
	}
	return nil
}

func markerItem(m *entity.Marker) (entity.MetadataItem, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return entity.MetadataItem{}, err
	}
	return entity.MetadataItem{Key: entity.MarkerKey, Value: b}, nil
}

// classify decides, from the listing and the asset's metadata, whether an
// asset is a candidate. It returns the marker instead when the asset is the
// unfinished copy of an interrupted replacement.
func (s *Service) classify(ctx context.Context, me string, listed entity.LibraryAsset) ([]entity.MetadataItem, *entity.Marker, error) {
	if err := basicChecks(me, listed); err != nil {
		return nil, nil, err
	}

	items, err := s.api.GetMetadata(ctx, listed.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading asset metadata: %w", err)
	}
	if m := markerOf(items); m != nil {
		if m.State == entity.MarkerStatePending {
			return nil, m, nil
		}
		return nil, nil, &errSkip{reason: "it was already handled by the optimizer", converted: m}
	}
	if ok, reason := s.conv.Converts(listed.OriginalFileName); !ok {
		return nil, nil, skip("%s", reason)
	}
	return items, nil, nil
}

// secondCopy handles an unconverted asset whose file is the original of an
// asset that is already converted. It is moved to the trash, unless it has
// something the converted asset lacks: then it is left for the user.
func (s *Service) secondCopy(ctx context.Context, listed entity.LibraryAsset, convertedID string, t *tally) error {
	t.item.NewID = convertedID
	old, err := s.api.GetAsset(ctx, listed.ID)
	if err != nil {
		return fmt.Errorf("reading asset: %w", err)
	}
	if old.IsTrashed {
		return skip("it is in the trash")
	}
	// Both are read again: the listing is from the start of the run.
	conv, err := s.api.GetAsset(ctx, convertedID)
	if errors.Is(err, entity.ErrAssetNotFound) {
		// Gone, or another user's: the audit log is shared by everyone
		// library mode works for, and it is only a cache.
		return errNotSecondCopy
	}
	if err != nil {
		return fmt.Errorf("reading the converted asset %s: %w", convertedID, err)
	}
	convItems, err := s.api.GetMetadata(ctx, convertedID)
	if err != nil {
		return fmt.Errorf("reading the metadata of the converted asset %s: %w", convertedID, err)
	}
	m := markerOf(convItems)
	if conv.IsTrashed || m == nil || m.State == entity.MarkerStatePending || m.Kept ||
		old.Checksum == "" || !strings.EqualFold(m.SHA1, old.Checksum) {
		return later("it looked like a second copy of the converted asset %s, which has changed since; a later run decides", convertedID)
	}
	t.item.NewFile, t.item.NewSize = conv.OriginalFileName, conv.Size

	var extra []string
	if old.IsFavorite && !conv.IsFavorite {
		extra = append(extra, "it is a favorite")
	}
	if old.StackID != "" && old.StackID != conv.StackID {
		extra = append(extra, "it is in a stack")
	}
	if old.Visibility != conv.Visibility {
		extra = append(extra, fmt.Sprintf("its visibility is %q", old.Visibility))
	}
	if old.Description != "" && old.Description != conv.Description {
		extra = append(extra, "it has a description")
	}
	if old.Rating != nil && !sameFloat(old.Rating, conv.Rating) {
		extra = append(extra, "it has a rating")
	}
	if len(minus(old.TagIDs, conv.TagIDs)) > 0 {
		extra = append(extra, "it has tags")
	}
	if old.IsEdited {
		extra = append(extra, "it has edits")
	}
	oldAlbums, err := s.api.Albums(ctx, old.ID)
	if err != nil {
		return fmt.Errorf("listing its albums: %w", err)
	}
	convAlbums, err := s.api.Albums(ctx, conv.ID)
	if err != nil {
		return fmt.Errorf("listing the albums of the converted asset: %w", err)
	}
	if len(minus(albumIDs(oldAlbums), albumIDs(convAlbums))) > 0 {
		extra = append(extra, "it is in albums")
	}
	links, err := s.api.SharedLinkIDs(ctx, old.ID)
	if err != nil {
		return fmt.Errorf("listing its shared links: %w", err)
	}
	if len(links) > 0 {
		extra = append(extra, "it is in shared links")
	}
	if len(extra) > 0 {
		return &errSkip{reason: fmt.Sprintf("it is a second copy of the converted asset %s, but %s that the converted one lacks: merge them by hand", convertedID, strings.Join(extra, ", ")), review: true}
	}

	t.item.Size = old.Size
	t.item.Reason = "a second copy of an original that is already converted"
	if err := s.api.Trash(ctx, []string{old.ID}); err != nil {
		return fmt.Errorf("moving it to the trash: %w", err)
	}
	t.SecondCopies++
	t.item.Status = statusSecondCopy
	s.infof("Library: moved %s (%s) to the trash: its conversion %s is already there", old.OriginalFileName, old.ID, convertedID)
	return s.record(Entry{Event: "second_copy_trashed", Asset: old.ID, File: old.OriginalFileName, NewAsset: convertedID, OriginalBytes: old.Size, OriginalSHA1: old.Checksum})
}

// work reads a candidate in full and replaces it (or, in a dry run, reports
// that it would).
func (s *Service) work(ctx context.Context, listed entity.LibraryAsset, items []entity.MetadataItem, t *tally) error {
	old, err := s.api.GetAsset(ctx, listed.ID)
	if err != nil {
		return fmt.Errorf("reading asset: %w", err)
	}
	if !old.Ready {
		return later("Immich has not finished reading it yet")
	}
	if old.IsTrashed {
		return later("it is in the trash")
	}
	albums, err := s.api.Albums(ctx, old.ID)
	if err != nil {
		return fmt.Errorf("listing its albums: %w", err)
	}
	for _, al := range albums {
		if !al.Shared {
			continue
		}
		n, err := s.api.ActivityCount(ctx, al.ID, old.ID)
		if err != nil {
			return fmt.Errorf("reading activity in album %q: %w", al.Name, err)
		}
		if n > 0 {
			return skip("it has comments or likes in the shared album %q, which cannot be carried over", al.Name)
		}
	}

	defer s.lockStack(old.StackID)()
	return s.replace(ctx, old, items, albums, t)
}

// replace converts one asset and swaps it in.
func (s *Service) replace(ctx context.Context, old *entity.LibraryAsset, oldItems []entity.MetadataItem, oldAlbums []entity.LibraryAlbum, t *tally) error {
	work, err := os.MkdirTemp("", "io-library-*")
	if err != nil {
		return fmt.Errorf("creating work folder: %w", err)
	}
	defer os.RemoveAll(work)

	// 1. Download and check against Immich's own checksum.
	ext := filepath.Ext(old.OriginalFileName)
	if strings.ContainsAny(ext, `/\`) || len(ext) > 32 {
		ext = ""
	}
	src := filepath.Join(work, "original"+ext)
	sum1, err := s.api.DownloadOriginal(ctx, old.ID, src)
	if err != nil {
		return fmt.Errorf("downloading the original: %w", err)
	}
	if sum1 != old.Checksum {
		return fmt.Errorf("downloaded file does not match Immich's checksum (%s != %s)", sum1, old.Checksum)
	}

	// 2. Convert and prove the result.
	in := entity.ConvertInput{
		Path:       src,
		Filename:   old.OriginalFileName,
		Source:     entity.SourceLibrary,
		ModTime:    old.FileModifiedAt,
		SHA1:       sum1,
		SkipUndone: true,
	}
	res, err := s.conv.Convert(ctx, in)
	if err != nil {
		return fmt.Errorf("converting: %w", err)
	}
	defer res.Release()

	if res.Outcome != entity.OutcomeOptimized || res.Marker == nil {
		return s.keep(ctx, old, oldItems, t, "conversion gave no size reduction")
	}
	// Motion photos (a photo with an embedded video). Immich extracts the
	// video of such an upload into an asset of its own and pairs it with the
	// photo, so converting only the photo loses nothing as long as that
	// pairing exists: the copy is paired with the same video.
	switch {
	case res.Marker.MotionPhotos == entity.MotionPhotosDropPhoto:
		return s.keep(ctx, old, oldItems, t, "motion photo whose task keeps only the video")
	case res.Marker.MotionPhotos == entity.MotionPhotosDropVideo && old.LivePhotoVideoID == "":
		return s.keep(ctx, old, oldItems, t, "motion photo whose video Immich did not extract: converting it would lose the video")
	case res.Marker.MotionPhotos == "" && s.cfg.IsMotionPhoto != nil && s.cfg.IsMotionPhoto(src):
		return s.keep(ctx, old, oldItems, t, "motion photo converted as a whole (motion_photos: keep): the result would hide its video")
	}
	if err := s.conv.CheckOutput(ctx, in, res); err != nil {
		return fmt.Errorf("the converted file does not look valid: %w", err)
	}
	convertedSHA1, err := fileSHA1(res.DeliverPath)
	if err != nil {
		return fmt.Errorf("hashing the converted file: %w", err)
	}
	entry := Entry{
		Asset: old.ID, File: old.OriginalFileName, Task: res.Marker.Task,
		OriginalBytes: res.OriginalSize, ConvertedBytes: res.OutputSize,
		OriginalSHA1: sum1, ConvertedSHA1: convertedSHA1,
	}
	e := entry
	e.Event = "converted"
	if err := s.record(e); err != nil {
		return err
	}

	// 3. Upload as a new asset, marked pending, carrying the original's own
	// metadata items.
	marker := *res.Marker
	marker.Replaces = old.ID
	marker.State = entity.MarkerStatePending
	mi, err := markerItem(&marker)
	if err != nil {
		return err
	}
	metadata := make([]entity.MetadataItem, 0, len(oldItems)+1)
	for _, it := range oldItems {
		if it.Key != entity.MarkerKey {
			metadata = append(metadata, it)
		}
	}
	metadata = append(metadata, mi)

	up, err := s.api.Upload(ctx, entity.LibraryUpload{
		Path:           res.DeliverPath,
		Filename:       res.DeliverName,
		FileCreatedAt:  old.FileCreatedAt,
		FileModifiedAt: old.FileModifiedAt,
		IsFavorite:     old.IsFavorite,
		Visibility:     old.Visibility,
		Metadata:       metadata,
	})
	if err != nil {
		return fmt.Errorf("uploading the converted file: %w", err)
	}
	if up.Duplicate {
		// Immich already holds these exact bytes. That is fine only when it
		// is our own unfinished replacement of this very asset.
		existing, err := s.api.GetMetadata(ctx, up.ID)
		if err != nil {
			return fmt.Errorf("reading the existing converted asset %s: %w", up.ID, err)
		}
		m := markerOf(existing)
		if m == nil || m.State != entity.MarkerStatePending || m.Replaces != old.ID {
			return skip("Immich already has the converted file as asset %s", up.ID)
		}
		s.infof("Library: resuming the unfinished replacement of %s (%s)", old.OriginalFileName, old.ID)
	}
	newID := up.ID
	e = entry
	e.Event, e.NewAsset = "uploaded", newID
	if err := s.record(e); err != nil {
		// No record, no change: remove what was just created.
		_ = s.api.Delete(context.WithoutCancel(ctx), []string{newID})
		return err
	}

	// 4-6. Carry over, verify, trash. Any failure removes the new asset.
	if err := s.finish(ctx, old, oldAlbums, newID, convertedSHA1, entry); err != nil {
		var fatal *errFatal
		if errors.As(err, &fatal) {
			return err
		}
		rb := entry
		rb.Event, rb.NewAsset, rb.Detail = "rolled_back", newID, err.Error()
		if derr := s.api.Delete(context.WithoutCancel(ctx), []string{newID}); derr != nil {
			rb.Event = "rollback_failed"
			rb.Detail = fmt.Sprintf("%v; and deleting the new asset failed: %v. The next run will remove it.", err, derr)
		} else {
			t.RolledBack++
		}
		if rerr := s.record(rb); rerr != nil {
			return rerr
		}
		var changed *errChanged
		if errors.As(err, &changed) && rb.Event == "rolled_back" {
			s.infof("Library: %s (%s) changed while it was being replaced; left as it is for a later run", old.OriginalFileName, old.ID)
			return later("%s; a later run will convert it", changed.what)
		}
		return fmt.Errorf("replacement abandoned, original untouched: %w", err)
	}

	// 7. The original is in the trash: the new asset is final.
	marker.State = ""
	final, err := markerItem(&marker)
	if err != nil {
		return err
	}
	if err := s.api.PutMetadata(context.WithoutCancel(ctx), newID, []entity.MetadataItem{final}); err != nil {
		// Harmless: the next run sees "pending" with a trashed original and
		// finishes this.
		if s.log != nil {
			s.log.Warnf("Library: could not clear the pending mark on %s, the next run will: %v", newID, err)
		}
	}
	t.Replaced++
	t.BytesBefore += res.OriginalSize
	t.BytesAfter += res.OutputSize
	t.item.Status, t.item.NewID, t.item.Task = statusReplaced, newID, res.Marker.Task
	t.item.NewFile = res.DeliverName
	t.item.Size, t.item.NewSize = res.OriginalSize, res.OutputSize
	s.infof("Library: replaced %s (%s) with %s (%s): %d -> %d bytes; the original is in the trash",
		old.OriginalFileName, old.ID, res.DeliverName, newID, res.OriginalSize, res.OutputSize)
	e = entry
	e.Event, e.NewAsset = "replaced", newID
	return s.record(e)
}

// keep records on the original that it was looked at and left as it is, so
// later runs do not download it again.
func (s *Service) keep(ctx context.Context, old *entity.LibraryAsset, oldItems []entity.MetadataItem, t *tally, note string) error {
	if err := s.writeKept(ctx, old, note); err != nil {
		return err
	}
	t.Kept++
	t.item.Status, t.item.Reason = statusKept, note
	s.infof("Library: keeping %s (%s) as it is: %s", old.OriginalFileName, old.ID, note)
	return s.record(Entry{Event: "kept", Asset: old.ID, File: old.OriginalFileName, Detail: note, OriginalBytes: old.Size})
}

// writeKept writes the marker of an asset that is left as it is.
func (s *Service) writeKept(ctx context.Context, old *entity.LibraryAsset, note string) error {
	mi, err := markerItem(&entity.Marker{
		V: entity.MarkerVersion, SHA1: old.Checksum, Size: old.Size, OriginalName: old.OriginalFileName,
		Source: string(entity.SourceLibrary), Optimizer: s.cfg.Version, ConfigSHA256: s.cfg.ConfigSHA256,
		At: time.Now(), Kept: true, Note: note,
	})
	if err != nil {
		return err
	}
	if err := s.api.PutMetadata(ctx, old.ID, []entity.MetadataItem{mi}); err != nil {
		return fmt.Errorf("recording that the asset was kept: %w", err)
	}
	return nil
}

// finish carries everything over to the new asset, verifies it and moves the
// original to the trash. On error the caller removes the new asset.
func (s *Service) finish(ctx context.Context, old *entity.LibraryAsset, oldAlbums []entity.LibraryAlbum, newID, convertedSHA1 string, entry Entry) error {
	if err := s.api.CopyAsset(ctx, old.ID, newID); err != nil {
		return fmt.Errorf("copying albums, links, stack and sidecar: %w", err)
	}
	cur, err := s.waitReady(ctx, newID)
	if err != nil {
		return err
	}
	// Immich's copy brings the original's tags along through the sidecar,
	// the optimizer's own among them, once it has read the new file: those
	// are about the original only.
	if err := s.dropOwnTags(ctx, newID); err != nil {
		return err
	}
	if cur.Checksum != convertedSHA1 {
		return fmt.Errorf("stored bytes differ from the uploaded ones (%s != %s)", cur.Checksum, convertedSHA1)
	}

	oldLinks, err := s.api.SharedLinkIDs(ctx, old.ID)
	if err != nil {
		return fmt.Errorf("listing shared links: %w", err)
	}
	oldFaces, err := s.api.Faces(ctx, old.ID)
	if err != nil {
		return fmt.Errorf("listing faces: %w", err)
	}
	if err := s.apply(ctx, old, cur, oldLinks, oldFaces); err != nil {
		return err
	}

	// Compare, giving Immich time to settle what was just applied.
	deadline := time.Now().Add(s.cfg.ReadyTimeout)
	var diffs []string
	for {
		diffs, err = s.compare(ctx, old, oldAlbums, oldLinks, newID)
		if err != nil {
			return err
		}
		if len(diffs) == 0 || time.Now().After(deadline) {
			break
		}
		if err := sleep(ctx, s.cfg.PollInterval); err != nil {
			return err
		}
	}
	if len(diffs) > 0 {
		e := entry
		e.Event, e.NewAsset, e.Differences = "verify_failed", newID, diffs
		if rerr := s.record(e); rerr != nil {
			return rerr
		}
		return fmt.Errorf("the new asset does not match the original: %s", strings.Join(diffs, "; "))
	}

	// The original must not have changed while this was going on.
	now, err := s.api.GetAsset(ctx, old.ID)
	if err != nil {
		return fmt.Errorf("re-reading the original: %w", err)
	}
	if now.IsTrashed {
		return &errChanged{"the original was moved to the trash meanwhile"}
	}
	if d := assetDiffs(old, now, false); len(d) > 0 {
		return &errChanged{"the original changed while it was being replaced (" + strings.Join(d, "; ") + ")"}
	}

	e := entry
	e.Event, e.NewAsset = "verified", newID
	if err := s.record(e); err != nil {
		return err
	}
	// From here the swap is committed even if the process is interrupted.
	if err := s.api.Trash(context.WithoutCancel(ctx), []string{old.ID}); err != nil {
		return fmt.Errorf("moving the original to the trash: %w", err)
	}
	return nil
}

func (s *Service) waitReady(ctx context.Context, id string) (*entity.LibraryAsset, error) {
	deadline := time.Now().Add(s.cfg.ReadyTimeout)
	for {
		a, err := s.api.GetAsset(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("reading the new asset: %w", err)
		}
		if a.Ready {
			return a, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for Immich to finish reading the new asset", s.cfg.ReadyTimeout)
		}
		if err := sleep(ctx, s.cfg.PollInterval); err != nil {
			return nil, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// apply sets on the new asset whatever the copy and Immich's own reading of
// the file did not already make equal to the original.
func (s *Service) apply(ctx context.Context, old, cur *entity.LibraryAsset, oldLinks []string, oldFaces []entity.LibraryFace) error {
	var u entity.LibraryAssetUpdate
	if cur.Description != old.Description {
		u.Description = &old.Description
	}
	if !sameFloat(cur.Rating, old.Rating) && old.Rating != nil {
		u.Rating = old.Rating
	}
	if cur.DateTimeOriginal != old.DateTimeOriginal && old.DateTimeOriginal != "" {
		u.DateTimeOriginal = &old.DateTimeOriginal
	}
	if (!sameFloat(cur.Latitude, old.Latitude) || !sameFloat(cur.Longitude, old.Longitude)) && old.Latitude != nil && old.Longitude != nil {
		u.Latitude, u.Longitude = old.Latitude, old.Longitude
	}
	if cur.IsFavorite != old.IsFavorite {
		u.IsFavorite = &old.IsFavorite
	}
	if cur.Visibility != old.Visibility {
		u.Visibility = &old.Visibility
	}
	if err := s.api.UpdateAsset(ctx, cur.ID, u); err != nil {
		return fmt.Errorf("carrying over edited fields: %w", err)
	}

	if missing := minus(old.TagIDs, cur.TagIDs); len(missing) > 0 {
		if err := s.api.TagAsset(ctx, cur.ID, missing); err != nil {
			return fmt.Errorf("carrying over tags: %w", err)
		}
	}

	have := map[string]bool{}
	for _, p := range cur.PersonIDs {
		have[p] = true
	}
	for _, f := range oldFaces {
		if have[f.PersonID] {
			continue
		}
		if err := s.api.CreateFace(ctx, cur.ID, f); err != nil {
			return fmt.Errorf("carrying over a face: %w", err)
		}
		have[f.PersonID] = true
	}

	newLinks, err := s.api.SharedLinkIDs(ctx, cur.ID)
	if err != nil {
		return fmt.Errorf("listing shared links: %w", err)
	}
	for _, id := range minus(oldLinks, newLinks) {
		if err := s.api.AddToSharedLink(ctx, id, cur.ID); err != nil {
			return fmt.Errorf("carrying over a shared link: %w", err)
		}
	}

	for _, id := range s.memories[old.ID] {
		if err := s.api.AddToMemory(ctx, id, cur.ID); err != nil {
			return fmt.Errorf("carrying over a memory: %w", err)
		}
	}

	if old.StackID != "" && old.StackPrimaryID == old.ID {
		if err := s.api.SetStackPrimary(ctx, old.StackID, cur.ID); err != nil {
			return fmt.Errorf("making the new asset the stack's primary: %w", err)
		}
	}

	// The video a live or motion photo is paired with.
	if old.LivePhotoVideoID != "" && cur.LivePhotoVideoID != old.LivePhotoVideoID {
		if err := s.api.UpdateAsset(ctx, cur.ID, entity.LibraryAssetUpdate{LivePhotoVideoID: &old.LivePhotoVideoID}); err != nil {
			return fmt.Errorf("pairing the copy with the photo's video: %w", err)
		}
	}

	// Edits made in Immich (crop, rotate, mirror) are stored as coordinates
	// on the original picture, so they only fit a copy of the same size.
	if old.IsEdited {
		edits, err := s.api.Edits(ctx, old.ID)
		if err != nil {
			return fmt.Errorf("reading the original's edits: %w", err)
		}
		if len(edits) > 0 {
			if cur.Width != old.Width || cur.Height != old.Height {
				return fmt.Errorf("the original has edits and the conversion changed the picture's size (%dx%d to %dx%d), so they cannot be carried over",
					old.Width, old.Height, cur.Width, cur.Height)
			}
			if err := s.api.SetEdits(ctx, cur.ID, edits); err != nil {
				return fmt.Errorf("carrying over edits: %w", err)
			}
		}
	}
	return nil
}

// compare returns every way the new asset differs from the original.
func (s *Service) compare(ctx context.Context, old *entity.LibraryAsset, oldAlbums []entity.LibraryAlbum, oldLinks []string, newID string) ([]string, error) {
	cur, err := s.api.GetAsset(ctx, newID)
	if err != nil {
		return nil, fmt.Errorf("reading the new asset: %w", err)
	}
	diffs := assetDiffs(old, cur, true)

	if old.IsEdited || cur.IsEdited {
		want, err := s.api.Edits(ctx, old.ID)
		if err != nil {
			return nil, fmt.Errorf("reading the original's edits: %w", err)
		}
		got, err := s.api.Edits(ctx, newID)
		if err != nil {
			return nil, fmt.Errorf("reading the new asset's edits: %w", err)
		}
		if !sameEdits(want, got) {
			diffs = append(diffs, "edits differ")
		}
	}

	newAlbums, err := s.api.Albums(ctx, newID)
	if err != nil {
		return nil, fmt.Errorf("listing the new asset's albums: %w", err)
	}
	if missing := minus(albumIDs(oldAlbums), albumIDs(newAlbums)); len(missing) > 0 {
		diffs = append(diffs, fmt.Sprintf("missing from %d album(s)", len(missing)))
	}
	newLinks, err := s.api.SharedLinkIDs(ctx, newID)
	if err != nil {
		return nil, fmt.Errorf("listing the new asset's shared links: %w", err)
	}
	if missing := minus(oldLinks, newLinks); len(missing) > 0 {
		diffs = append(diffs, fmt.Sprintf("missing from %d shared link(s)", len(missing)))
	}
	return diffs, nil
}

// sameEdits compares two edit lists by action and parameters, in order.
func sameEdits(a, b []entity.LibraryEdit) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		var x, y any
		if a[i].Action != b[i].Action || json.Unmarshal(a[i].Parameters, &x) != nil || json.Unmarshal(b[i].Parameters, &y) != nil {
			return false
		}
		if fmt.Sprint(x) != fmt.Sprint(y) {
			return false
		}
	}
	return true
}

func albumIDs(albums []entity.LibraryAlbum) []string {
	ids := make([]string, 0, len(albums))
	for _, a := range albums {
		ids = append(ids, a.ID)
	}
	return ids
}

// minus returns the elements of a that are not in b.
func minus(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, x := range b {
		in[x] = true
	}
	var out []string
	for _, x := range a {
		if !in[x] {
			out = append(out, x)
		}
	}
	return out
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	d := *a - *b
	return d < 1e-6 && d > -1e-6
}

func stem(name string) string { return strings.TrimSuffix(name, filepath.Ext(name)) }

// assetDiffs lists how b differs from a. With replacement set, b is the
// converted copy of a: its file name may differ in extension only, it may
// belong to more people (Immich can detect additional faces), and its place
// in the stack is checked. Without it, a and b are two readings of the same
// asset and must be identical.
func assetDiffs(a, b *entity.LibraryAsset, replacement bool) []string {
	var d []string
	add := func(field string, x, y any) {
		d = append(d, fmt.Sprintf("%s: %v != %v", field, x, y))
	}
	check := func(field string, x, y string) {
		if x != y {
			add(field, x, y)
		}
	}
	if replacement {
		check("file name", stem(a.OriginalFileName), stem(b.OriginalFileName))
	} else {
		check("file name", a.OriginalFileName, b.OriginalFileName)
		check("checksum", a.Checksum, b.Checksum)
	}
	check("owner", a.OwnerID, b.OwnerID)
	check("paired video", a.LivePhotoVideoID, b.LivePhotoVideoID)
	check("type", a.Type, b.Type)
	check("visibility", a.Visibility, b.Visibility)
	if !replacement {
		check("duration", a.Duration, b.Duration)
	} else if !similarDuration(a.Duration, b.Duration) {
		add("duration", a.Duration, b.Duration)
	}
	check("date taken", a.DateTimeOriginal, b.DateTimeOriginal)
	check("time zone", a.TimeZone, b.TimeZone)
	check("description", a.Description, b.Description)
	check("camera make", a.Make, b.Make)
	check("camera model", a.Model, b.Model)
	check("lens", a.LensModel, b.LensModel)
	check("projection", a.ProjectionType, b.ProjectionType)
	if !a.FileCreatedAt.Equal(b.FileCreatedAt) {
		add("file created", a.FileCreatedAt, b.FileCreatedAt)
	}
	if !a.FileModifiedAt.Equal(b.FileModifiedAt) {
		add("file modified", a.FileModifiedAt, b.FileModifiedAt)
	}
	if !a.LocalDateTime.Equal(b.LocalDateTime) {
		add("local date", a.LocalDateTime, b.LocalDateTime)
	}
	if a.IsFavorite != b.IsFavorite {
		add("favorite", a.IsFavorite, b.IsFavorite)
	}
	// A task may crop, resize or bake the rotation in, so the picture's
	// shape is only compared between two readings of the same asset.
	if !replacement {
		check("orientation", a.Orientation, b.Orientation)
		if a.Width != b.Width || a.Height != b.Height {
			add("dimensions", fmt.Sprintf("%dx%d", a.Width, a.Height), fmt.Sprintf("%dx%d", b.Width, b.Height))
		}
	}
	if !sameFloat(a.Latitude, b.Latitude) || !sameFloat(a.Longitude, b.Longitude) {
		add("location", fmtPos(a), fmtPos(b))
	}
	if !sameFloat(a.Rating, b.Rating) {
		add("rating", fmtFloat(a.Rating), fmtFloat(b.Rating))
	}
	if m := minus(a.TagIDs, b.TagIDs); len(m) > 0 {
		d = append(d, fmt.Sprintf("missing %d tag(s)", len(m)))
	}
	if m := minus(a.PersonIDs, b.PersonIDs); len(m) > 0 {
		d = append(d, fmt.Sprintf("missing %d person(s)", len(m)))
	}
	if replacement {
		if a.StackID != b.StackID {
			add("stack", a.StackID, b.StackID)
		} else if a.StackID != "" && a.StackPrimaryID == a.ID && b.StackPrimaryID != b.ID {
			d = append(d, "the new asset is not the stack's primary")
		}
	} else {
		if a.IsEdited != b.IsEdited {
			add("edited", a.IsEdited, b.IsEdited)
		}
		if m := minus(b.TagIDs, a.TagIDs); len(m) > 0 {
			d = append(d, fmt.Sprintf("%d tag(s) added", len(m)))
		}
		if m := minus(b.PersonIDs, a.PersonIDs); len(m) > 0 {
			d = append(d, fmt.Sprintf("%d person(s) added", len(m)))
		}
	}
	return d
}

// similarDuration accepts the small length change a re-encode causes: half a
// second or 2%, whichever is larger, when both are plain numbers.
func similarDuration(a, b string) bool {
	if a == b {
		return true
	}
	x, errX := strconv.ParseFloat(a, 64)
	y, errY := strconv.ParseFloat(b, 64)
	if errX != nil || errY != nil {
		return false
	}
	tol := math.Max(500, 0.02*math.Max(x, y)) // milliseconds
	return math.Abs(x-y) <= tol
}

func fmtFloat(f *float64) string {
	if f == nil {
		return "none"
	}
	return fmt.Sprintf("%g", *f)
}

func fmtPos(a *entity.LibraryAsset) string {
	return fmtFloat(a.Latitude) + "," + fmtFloat(a.Longitude)
}

// resumePending deals with a converted asset whose replacement was
// interrupted: if its original is already gone or in the trash the swap had
// been committed and only the mark is cleared; otherwise the half-made copy
// is removed and the original is converted afresh when the run reaches it.
func (s *Service) resumePending(ctx context.Context, pending entity.LibraryAsset, m *entity.Marker, t *tally) error {
	committed := m.Replaces == ""
	if !committed {
		old, err := s.api.GetAsset(ctx, m.Replaces)
		switch {
		case err == nil:
			committed = old.IsTrashed
		case errors.Is(err, entity.ErrAssetNotFound):
			committed = true // the original no longer exists
		default:
			return fmt.Errorf("reading the original %s of an unfinished replacement: %w", m.Replaces, err)
		}
	}
	if committed {
		final := *m
		final.State = ""
		mi, err := markerItem(&final)
		if err != nil {
			return err
		}
		if err := s.api.PutMetadata(ctx, pending.ID, []entity.MetadataItem{mi}); err != nil {
			return fmt.Errorf("clearing the pending mark: %w", err)
		}
		t.Finalized++
		t.item.Status, t.item.Reason = statusFinalized, "replacement of "+m.Replaces+" from an earlier run completed"
		s.infof("Library: finished the interrupted replacement of %s by %s", m.Replaces, pending.ID)
		return s.record(Entry{Event: "finalized", Asset: m.Replaces, NewAsset: pending.ID, File: pending.OriginalFileName})
	}
	if err := s.api.Delete(ctx, []string{pending.ID}); err != nil {
		return fmt.Errorf("removing the unfinished copy %s: %w", pending.ID, err)
	}
	t.RolledBack++
	t.item.Status, t.item.Reason = statusRolledBack, "unfinished copy of "+m.Replaces+" from an earlier run removed"
	s.infof("Library: removed the unfinished copy %s; its original %s is untouched", pending.ID, m.Replaces)
	return s.record(Entry{Event: "rolled_back", Asset: m.Replaces, NewAsset: pending.ID, File: pending.OriginalFileName,
		Detail: "unfinished replacement from an earlier run"})
}

func fileSHA1(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New() // #nosec G401 -- content identity matching Immich, not security
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
