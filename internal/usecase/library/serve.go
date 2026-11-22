package library

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// SafetyInterval is how often Serve looks for tagged assets without being
// told to: Immich's events are what normally starts a pass, and this catches
// whatever an event did not.
const SafetyInterval = 15 * time.Minute

// settleDelay lets a burst of events (tagging a whole album) end before a
// pass starts.
var settleDelay = 2 * time.Second

// retryDelay is how long Serve waits before trying again to start, when
// Immich cannot be reached.
var retryDelay = 30 * time.Second

// Serve runs library mode until ctx ends: it reacts to Immich's events,
// with SafetyInterval as a fallback, and returns when ctx ends. A pass that
// cannot write the audit log ends there; the next one tries again.
//
// At startup it settles what an interrupted run left unfinished and acts on
// whatever is tagged. Which unfinished copies exist, and which originals were
// converted into what, comes from the audit log; only when the log knows
// nothing is the whole library read (see Run).
func (s *Service) Serve(ctx context.Context, events port.LibraryEvents) error {
	defer func() { s.idx, s.uploadsChecked = nil, time.Time{} }()
	defer s.cfg.Users.release(s)

	// Start: retried until Immich answers.
	var me string
	for {
		err := s.start(ctx, &me)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		// A setting that cannot work does not start working by waiting.
		var conf *errConfig
		if errors.As(err, &conf) {
			return err
		}
		if s.log != nil {
			s.log.Errorf("Library: %v; trying again in %s", err, retryDelay)
		}
		if sleep(ctx, retryDelay) != nil {
			return nil
		}
	}

	wake := make(chan struct{}, 1)
	poke := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	// recheck asks for a look at the recent uploads: events for them may
	// have been missed.
	recheck := make(chan struct{}, 1)
	missed := func() {
		select {
		case recheck <- struct{}{}:
		default:
		}
	}
	type upload struct{ id, checksum string }
	uploads := make(chan upload, 1024)
	if events != nil {
		go events.Listen(ctx, port.LibraryEventHandlers{
			// Also after a lost connection: what was uploaded meanwhile was
			// not announced.
			Connected: func() { poke(); missed() },
			AssetUpdated: func(_ string, tags []string) {
				for _, t := range tags {
					if t == TagConvert || strings.HasPrefix(t, TagConvert+"/") {
						poke()
						return
					}
				}
			},
			Uploaded: func(id, checksum string) {
				select {
				case uploads <- upload{id, checksum}:
				default: // a flood of uploads: they are looked for afterwards
					missed()
				}
			},
		})
	}
	safety := time.NewTicker(SafetyInterval)
	defer safety.Stop()
	s.infof("Library: waiting for tags (%s)", strings.Join(s.commandTags(), ", "))
	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
			if sleep(ctx, settleDelay) != nil {
				return nil
			}
			select { // events during the delay are covered by this pass
			case <-wake:
			default:
			}
			err = s.taggedPass(ctx, me)
		case <-safety.C:
			// Also what a replacement left unfinished since the start, and
			// uploads whose event did not arrive.
			if err = s.pendingPass(ctx, me); err == nil {
				err = s.taggedPass(ctx, me)
			}
			if err == nil {
				err = s.recentPass(ctx, me)
			}
		case <-recheck:
			err = s.recentPass(ctx, me)
		case u := <-uploads:
			err = s.uploadPass(ctx, me, u.id, u.checksum)
		}
		// Also when the audit log could not be written: that pass is over,
		// and the next one finds out whether the log is back.
		var fatal *errFatal
		if err != nil && ctx.Err() == nil && s.log != nil {
			if errors.As(err, &fatal) {
				s.log.Errorf("Library: the audit log cannot be written, so nothing is converted until it can: %v", err)
			} else {
				s.log.Errorf("Library: %v", err)
			}
		}
	}
}

// Warnf logs a warning under the name of this worker: its key until the
// user is known, then the user's id.
func (s *Service) Warnf(format string, args ...any) {
	if s.log != nil {
		s.log.Warnf(format, args...)
	}
}

// errConfig is a start that fails because of the settings: Serve gives up
// instead of trying again.
type errConfig struct{ err error }

func (e *errConfig) Error() string { return e.err.Error() }
func (e *errConfig) Unwrap() error { return e.err }

// start identifies the user, settles what earlier runs left and acts on
// what is tagged.
func (s *Service) start(ctx context.Context, me *string) error {
	u, err := s.base.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("identifying the API key's user: %w", err)
	}
	id := u.ID
	if !s.cfg.Users.claim(id, s) {
		return &errConfig{fmt.Errorf("two API keys belong to user %s <%s>: give library mode one key per user", id, u.Email)}
	}
	if s.me != id {
		if s.cfg.Label != "" {
			s.log = s.rootLog.WithPrefix("[user " + id + "] ")
		}
		s.infof("Library: working for %s <%s> (user %s)", u.Name, u.Email, id)
	}
	*me, s.me = id, id
	if s.idx == nil {
		// The log is shared by everyone library mode works for: this
		// user's part of it.
		s.idx = newLogIndex(s.journal.Path(), id)
		if err := s.idx.refresh(); err != nil && s.log != nil {
			s.log.Warnf("Library: could not read the audit log (%v); reading the whole library instead", err)
		}
		// Uploads have been looked at for as long as library mode ran,
		// which the log shows.
		s.uploadsChecked = s.idx.lastSeen()
	}
	if err := s.tidyTags(ctx); err != nil {
		return err
	}
	if !s.idx.isKnown() {
		s.infof("Library: the audit log has no record of earlier conversions; reading the whole library once")
		began := time.Now()
		_, err := s.Run(ctx)
		if err == nil {
			s.uploadsChecked = began
		}
		return err
	}
	if err := s.pendingPass(ctx, id); err != nil {
		return err
	}
	if err := s.taggedPass(ctx, id); err != nil {
		return err
	}
	if err := s.recentPass(ctx, id); err != nil {
		return err
	}
	// So that the next start knows from when to look, also when nothing
	// else is written for a long time.
	return s.record(Entry{Event: eventUploadsChecked})
}

// eventUploadsChecked is logged at startup, once the uploads made while
// library mode was not running have been looked at.
const eventUploadsChecked = "uploads_checked"

// uploadCheckMargin is how far before the last check recentPass looks: the
// clocks of Immich and the optimizer may differ, and an upload takes a
// moment to appear.
const uploadCheckMargin = 10 * time.Minute

// recentPass looks at what was uploaded since uploads were last looked at,
// for second copies of converted originals. Immich announces every upload,
// but not to a listener that is not connected: while the optimizer was
// stopped or the connection was down, or when more arrived than it could
// take. One search, and no asset is read unless its checksum is that of a
// converted original.
func (s *Service) recentPass(ctx context.Context, me string) error {
	if err := s.idx.refresh(); err != nil && s.log != nil {
		s.log.Warnf("Library: could not read the audit log: %v", err)
	}
	now := time.Now()
	if !s.idx.hasOriginals() {
		s.uploadsChecked = now
		return nil
	}
	since := s.uploadsChecked
	if !since.IsZero() {
		since = since.Add(-uploadCheckMargin)
	}
	recent, err := s.base.AssetsCreatedSince(ctx, since)
	if err != nil {
		return fmt.Errorf("listing recent uploads: %w", err)
	}
	var copies []entity.LibraryAsset
	for _, a := range recent {
		if a.OwnerID != me || a.IsTrashed || a.Checksum == "" {
			continue
		}
		if conv, ok := s.idx.convertedFrom(a.Checksum); ok && conv != a.ID {
			copies = append(copies, a)
		}
	}
	if len(copies) > 0 {
		s.infof("Library: %d recent upload(s) may be second copies of converted originals; checking them", len(copies))
		rt, err := s.loadTags(ctx)
		if err != nil {
			return err
		}
		if _, err := s.pass(ctx, me, rt, copies); err != nil {
			return err
		}
	}
	s.uploadsChecked = now
	return nil
}

// uploadSettle is how long after its conversion an upload is still looked
// for among the recent uploads: a large video takes a while to send.
const uploadSettle = time.Hour

// pendingPass settles the copies the audit log lists as unfinished.
func (s *Service) pendingPass(ctx context.Context, me string) error {
	if err := s.idx.refresh(); err != nil && s.log != nil {
		s.log.Warnf("Library: could not read the audit log: %v", err)
	}
	ids := s.idx.pendingIDs()
	// A copy uploaded right before an interruption, whose upload the log
	// does not show: found by its checksum among the recent uploads.
	if sums, since := s.idx.uploadingSince(); len(sums) > 0 {
		looked := time.Now()
		recent, err := s.base.AssetsCreatedSince(ctx, since.Add(-uploadCheckMargin))
		if err != nil {
			return fmt.Errorf("listing recent uploads: %w", err)
		}
		for _, a := range recent {
			if a.OwnerID != me || !sums[strings.ToLower(a.Checksum)] {
				continue
			}
			// Only an unfinished replacement: the same bytes can also be
			// an asset of the person's own.
			items, err := s.base.GetMetadata(ctx, a.ID)
			if err != nil {
				return fmt.Errorf("reading the metadata of %s: %w", a.ID, err)
			}
			if markerOf(items).Unfinished() {
				ids = append(ids, a.ID)
			}
		}
		s.idx.forgetUploading(sums, looked.Add(-uploadSettle))
	}
	if len(ids) == 0 {
		return nil
	}
	assets, err := s.fetch(ctx, ids)
	if err != nil || len(assets) == 0 {
		return err
	}
	s.infof("Library: %d replacement(s) may have been interrupted; checking them", len(assets))
	rt, err := s.loadTags(ctx)
	if err != nil {
		return err
	}
	_, err = s.pass(ctx, me, rt, assets)
	return err
}

// taggedPass acts on every asset that carries a command tag.
func (s *Service) taggedPass(ctx context.Context, me string) error {
	rt, err := s.loadTags(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(rt.convert))
	for id := range rt.convert {
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	assets, err := s.fetch(ctx, ids)
	if err != nil {
		return err
	}
	_, err = s.pass(ctx, me, rt, assets)
	return err
}

// uploadPass looks at a new upload whose file the audit log knows as the
// original of a conversion: it is a second copy.
func (s *Service) uploadPass(ctx context.Context, me, id, checksum string) error {
	raw, err := base64.StdEncoding.DecodeString(checksum)
	if err != nil {
		return nil //nolint:nilerr // not a checksum: nothing to look for
	}
	if err := s.idx.refresh(); err != nil && s.log != nil {
		s.log.Warnf("Library: could not read the audit log: %v", err)
	}
	if conv, ok := s.idx.convertedFrom(hex.EncodeToString(raw)); !ok || conv == id {
		return nil
	}
	rt, err := s.loadTags(ctx)
	if err != nil {
		return err
	}
	assets, err := s.fetch(ctx, []string{id})
	if err != nil || len(assets) == 0 {
		return err
	}
	_, err = s.pass(ctx, me, rt, assets)
	return err
}

// fetch reads assets by id; ones that no longer exist are left out.
func (s *Service) fetch(ctx context.Context, ids []string) ([]entity.LibraryAsset, error) {
	sort.Strings(ids)
	ids = dedupe(ids)
	out := make([]entity.LibraryAsset, 0, len(ids))
	for _, id := range ids {
		a, err := s.base.GetAsset(ctx, id)
		if errors.Is(err, entity.ErrAssetNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading asset %s: %w", id, err)
		}
		out = append(out, *a)
	}
	return out, nil
}
