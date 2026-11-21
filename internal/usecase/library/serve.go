package library

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
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
// with SafetyInterval as a fallback, and returns only on an error it cannot
// continue after (the audit log cannot be written) or when ctx ends.
//
// At startup it settles what an interrupted run left unfinished and acts on
// whatever is tagged. Which unfinished copies exist, and which originals were
// converted into what, comes from the audit log; only when the log knows
// nothing is the whole library read (see Run).
func (s *Service) Serve(ctx context.Context, events port.LibraryEvents) error {
	defer func() { s.idx = nil }()

	// Start: retried until Immich answers.
	var me string
	for {
		err := s.start(ctx, &me)
		if err == nil {
			break
		}
		var fatal *errFatal
		if errors.As(err, &fatal) || ctx.Err() != nil {
			return ignoreCanceled(ctx, err)
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
	type upload struct{ id, checksum string }
	uploads := make(chan upload, 1024)
	if events != nil {
		go events.Listen(ctx, port.LibraryEventHandlers{
			Connected: poke,
			AssetUpdated: func(_ string, tags []string) {
				for _, t := range tags {
					if t == TagConvert || t == TagKeep || t == TagRedo || t == TagReconvert {
						poke()
						return
					}
				}
			},
			Uploaded: func(id, checksum string) {
				select {
				case uploads <- upload{id, checksum}:
				default: // a flood of uploads: the safety pass catches up
				}
			},
		})
	}
	safety := time.NewTicker(SafetyInterval)
	defer safety.Stop()
	s.infof("Library: waiting for tags (%s, %s, %s, %s)", TagConvert, TagKeep, TagRedo, TagReconvert)
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
			err = s.taggedPass(ctx, me)
		case u := <-uploads:
			err = s.uploadPass(ctx, me, u.id, u.checksum)
		}
		var fatal *errFatal
		if errors.As(err, &fatal) {
			return err
		}
		if err != nil && ctx.Err() == nil && s.log != nil {
			s.log.Errorf("Library: %v", err)
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

func ignoreCanceled(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// start identifies the user, settles what earlier runs left and acts on
// what is tagged.
func (s *Service) start(ctx context.Context, me *string) error {
	u, err := s.base.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("identifying the API key's user: %w", err)
	}
	id := u.ID
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
	}
	if !s.idx.isKnown() {
		s.infof("Library: the audit log has no record of earlier conversions; reading the whole library once")
		_, err := s.Run(ctx)
		return err
	}
	if ids := s.idx.pendingIDs(); len(ids) > 0 {
		s.infof("Library: %d replacement(s) may have been interrupted; checking them", len(ids))
		rt, err := s.loadTags(ctx)
		if err != nil {
			return err
		}
		assets, err := s.fetch(ctx, ids)
		if err != nil {
			return err
		}
		if _, err := s.pass(ctx, id, rt, assets); err != nil {
			return err
		}
	}
	return s.taggedPass(ctx, id)
}

// taggedPass acts on every asset that carries a command tag.
func (s *Service) taggedPass(ctx context.Context, me string) error {
	rt, err := s.loadTags(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(rt.convert)+len(rt.keep)+len(rt.redo)+len(rt.reconvert))
	for _, set := range []map[string]bool{rt.convert, rt.keep, rt.redo, rt.reconvert} {
		for id := range set {
			ids = append(ids, id)
		}
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
