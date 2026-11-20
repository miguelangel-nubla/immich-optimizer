// Package library converts the assets that already exist in an Immich
// library. Immich has no "replace this file" call, so each asset is replaced
// in a fixed order that never leaves the user without their photo:
//
//  1. download the original and check it against Immich's checksum
//  2. convert it, and prove the result with the task's verify command
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

type Config struct {
	// DryRun reports what would be converted without changing anything.
	DryRun bool
	// Limit stops the run after this many replacements; 0 means no limit.
	Limit int
	// TakenBefore, when set, restricts the run to assets whose file date is
	// older.
	TakenBefore time.Time
	// ReadyTimeout is how long to wait for Immich to finish reading a newly
	// uploaded asset before giving up on it.
	ReadyTimeout time.Duration
	// PollInterval is the delay between readiness checks.
	PollInterval time.Duration
	// Version is the optimizer version recorded in markers.
	Version string
}

// Summary counts what a run did.
type Summary struct {
	Seen         int   `json:"seen"`
	Replaced     int   `json:"replaced"`
	WouldReplace int   `json:"would_replace,omitempty"`
	Kept         int   `json:"kept"`
	Skipped      int   `json:"skipped"`
	Failed       int   `json:"failed"`
	Finalized    int   `json:"finalized,omitempty"`
	RolledBack   int   `json:"rolled_back,omitempty"`
	BytesBefore  int64 `json:"bytes_before"`
	BytesAfter   int64 `json:"bytes_after"`
}

type Service struct {
	cfg     Config
	api     port.LibraryAPI
	conv    port.VerifyingConverter
	log     *logger.Logger
	journal *Journal
}

func New(cfg Config, api port.LibraryAPI, conv port.VerifyingConverter, journal *Journal, log *logger.Logger) *Service {
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 10 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	return &Service{cfg: cfg, api: api, conv: conv, log: log, journal: journal}
}

// errSkip is returned for assets library mode leaves alone on purpose.
type errSkip struct{ reason string }

func (e *errSkip) Error() string { return e.reason }

func skip(format string, args ...any) error { return &errSkip{reason: fmt.Sprintf(format, args...)} }

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
	e.DryRun = s.cfg.DryRun
	if err := s.journal.Write(e); err != nil {
		return &errFatal{err}
	}
	return nil
}

// Run makes one pass over the library.
func (s *Service) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	me, err := s.api.CurrentUserID(ctx)
	if err != nil {
		return sum, fmt.Errorf("identifying the API key's user: %w", err)
	}
	if err := s.record(Entry{Event: "run_started", Detail: "user " + me}); err != nil {
		return sum, err
	}

	// List everything first: replacing assets while paging through them
	// would shift the pages.
	var assets []entity.LibraryAsset
	for page := 1; page != 0; {
		items, next, err := s.api.ListAssets(ctx, page)
		if err != nil {
			return sum, fmt.Errorf("listing assets: %w", err)
		}
		assets = append(assets, items...)
		page = next
	}
	s.infof("Library: %d asset(s) listed%s", len(assets), map[bool]string{true: " (dry run: nothing will be changed)"}[s.cfg.DryRun])

	for i := range assets {
		if ctx.Err() != nil {
			break
		}
		if s.cfg.Limit > 0 && sum.Replaced+sum.WouldReplace >= s.cfg.Limit {
			s.infof("Library: limit of %d replacement(s) reached", s.cfg.Limit)
			break
		}
		sum.Seen++
		a := assets[i]
		err := s.process(ctx, me, a, &sum)
		var sk *errSkip
		var fatal *errFatal
		switch {
		case err == nil:
		case errors.As(err, &fatal):
			return sum, fatal.err
		case ctx.Err() != nil:
			// Interrupted: whatever was in flight has been rolled back or
			// will be finished by the next run.
		case errors.As(err, &sk):
			sum.Skipped++
			if s.log != nil {
				s.log.Debugf("Library: skipping %s (%s): %s", a.OriginalFileName, a.ID, sk.reason)
			}
		default:
			sum.Failed++
			if s.log != nil {
				s.log.Errorf("Library: %s (%s) failed: %v", a.OriginalFileName, a.ID, err)
			}
			if rerr := s.record(Entry{Event: "failed", Asset: a.ID, File: a.OriginalFileName, Detail: err.Error()}); rerr != nil {
				return sum, rerr
			}
		}
	}

	if err := s.record(Entry{Event: "run_finished", Summary: &sum}); err != nil {
		return sum, err
	}
	s.infof("Library: run finished: %d seen, %d replaced, %d would be replaced, %d kept, %d skipped, %d failed (%d -> %d bytes)",
		sum.Seen, sum.Replaced, sum.WouldReplace, sum.Kept, sum.Skipped, sum.Failed, sum.BytesBefore, sum.BytesAfter)
	return sum, ctx.Err()
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

func (s *Service) process(ctx context.Context, me string, listed entity.LibraryAsset, sum *Summary) error {
	// Cheap checks on the listing first.
	switch {
	case listed.OwnerID != me:
		return skip("it belongs to another user")
	case listed.LibraryID != "":
		return skip("it is in an external library")
	case listed.IsTrashed:
		return skip("it is in the trash")
	case listed.IsOffline:
		return skip("its file is offline")
	case listed.Visibility != "timeline" && listed.Visibility != "archive":
		return skip("its visibility is %q", listed.Visibility)
	case !s.cfg.TakenBefore.IsZero() && !listed.FileCreatedAt.Before(s.cfg.TakenBefore):
		return skip("it is newer than the configured date")
	}

	items, err := s.api.GetMetadata(ctx, listed.ID)
	if err != nil {
		return fmt.Errorf("reading asset metadata: %w", err)
	}
	if m := markerOf(items); m != nil {
		if m.State == entity.MarkerStatePending {
			return s.resumePending(ctx, listed, m, sum)
		}
		return skip("it was already handled by the optimizer")
	}

	switch {
	case listed.LivePhotoVideoID != "":
		return skip("it is a live photo, which library mode does not handle yet")
	case listed.IsEdited:
		return skip("it has edits, which library mode does not handle yet")
	}
	if ok, reason := s.conv.Verifiable(listed.OriginalFileName); !ok {
		return skip("%s", reason)
	}

	old, err := s.api.GetAsset(ctx, listed.ID)
	if err != nil {
		return fmt.Errorf("reading asset: %w", err)
	}
	if !old.Ready {
		return skip("Immich has not finished reading it yet")
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

	if s.cfg.DryRun {
		sum.WouldReplace++
		sum.BytesBefore += old.Size
		s.infof("Library (dry run): would convert %s (%s)", old.OriginalFileName, old.ID)
		return s.record(Entry{Event: "would_convert", Asset: old.ID, File: old.OriginalFileName, OriginalBytes: old.Size})
	}
	return s.replace(ctx, old, items, albums, sum)
}

// replace converts one asset and swaps it in.
func (s *Service) replace(ctx context.Context, old *entity.LibraryAsset, oldItems []entity.MetadataItem, oldAlbums []entity.LibraryAlbum, sum *Summary) error {
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
		return s.keep(ctx, old, oldItems, sum, "conversion gave no size reduction")
	}
	if res.Marker.MotionPhotos != "" {
		return s.keep(ctx, old, oldItems, sum, "motion photo, which library mode does not handle yet")
	}
	if err := s.conv.Verify(ctx, in, res); err != nil {
		return fmt.Errorf("the converted file did not pass its task's verify command: %w", err)
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
			sum.RolledBack++
		}
		if rerr := s.record(rb); rerr != nil {
			return rerr
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
	sum.Replaced++
	sum.BytesBefore += res.OriginalSize
	sum.BytesAfter += res.OutputSize
	s.infof("Library: replaced %s (%s) with %s (%s): %d -> %d bytes; the original is in the trash",
		old.OriginalFileName, old.ID, res.DeliverName, newID, res.OriginalSize, res.OutputSize)
	e = entry
	e.Event, e.NewAsset = "replaced", newID
	return s.record(e)
}

// keep records on the original that it was looked at and left as it is, so
// later runs do not download it again.
func (s *Service) keep(ctx context.Context, old *entity.LibraryAsset, oldItems []entity.MetadataItem, sum *Summary, note string) error {
	mi, err := markerItem(&entity.Marker{
		V: 1, SHA1: old.Checksum, Size: old.Size, Source: string(entity.SourceLibrary),
		Optimizer: s.cfg.Version, Kept: true, Note: note,
	})
	if err != nil {
		return err
	}
	if err := s.api.PutMetadata(ctx, old.ID, []entity.MetadataItem{mi}); err != nil {
		return fmt.Errorf("recording that the asset was kept: %w", err)
	}
	sum.Kept++
	s.infof("Library: keeping %s (%s) as it is: %s", old.OriginalFileName, old.ID, note)
	return s.record(Entry{Event: "kept", Asset: old.ID, File: old.OriginalFileName, Detail: note, OriginalBytes: old.Size})
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
	if cur.Checksum != convertedSHA1 {
		return fmt.Errorf("Immich stored different bytes than were uploaded (%s != %s)", cur.Checksum, convertedSHA1)
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
		return fmt.Errorf("the original was moved to the trash by someone else meanwhile")
	}
	if d := assetDiffs(old, now, false); len(d) > 0 {
		return fmt.Errorf("the original changed during the replacement: %s", strings.Join(d, "; "))
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
			return nil, fmt.Errorf("Immich did not finish reading the new asset within %s", s.cfg.ReadyTimeout)
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

	if old.StackID != "" && old.StackPrimaryID == old.ID {
		if err := s.api.SetStackPrimary(ctx, old.StackID, cur.ID); err != nil {
			return fmt.Errorf("making the new asset the stack's primary: %w", err)
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
	check("type", a.Type, b.Type)
	check("visibility", a.Visibility, b.Visibility)
	check("duration", a.Duration, b.Duration)
	check("date taken", a.DateTimeOriginal, b.DateTimeOriginal)
	check("time zone", a.TimeZone, b.TimeZone)
	check("description", a.Description, b.Description)
	check("camera make", a.Make, b.Make)
	check("camera model", a.Model, b.Model)
	check("lens", a.LensModel, b.LensModel)
	check("orientation", a.Orientation, b.Orientation)
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
	if a.Width != b.Width || a.Height != b.Height {
		add("dimensions", fmt.Sprintf("%dx%d", a.Width, a.Height), fmt.Sprintf("%dx%d", b.Width, b.Height))
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
		if m := minus(b.TagIDs, a.TagIDs); len(m) > 0 {
			d = append(d, fmt.Sprintf("%d tag(s) added", len(m)))
		}
		if m := minus(b.PersonIDs, a.PersonIDs); len(m) > 0 {
			d = append(d, fmt.Sprintf("%d person(s) added", len(m)))
		}
	}
	return d
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
func (s *Service) resumePending(ctx context.Context, pending entity.LibraryAsset, m *entity.Marker, sum *Summary) error {
	if s.cfg.DryRun {
		return skip("it is an unfinished replacement of %s (a real run will resolve it)", m.Replaces)
	}
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
		sum.Finalized++
		s.infof("Library: finished the interrupted replacement of %s by %s", m.Replaces, pending.ID)
		return s.record(Entry{Event: "finalized", Asset: m.Replaces, NewAsset: pending.ID, File: pending.OriginalFileName})
	}
	if err := s.api.Delete(ctx, []string{pending.ID}); err != nil {
		return fmt.Errorf("removing the unfinished copy %s: %w", pending.ID, err)
	}
	sum.RolledBack++
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
