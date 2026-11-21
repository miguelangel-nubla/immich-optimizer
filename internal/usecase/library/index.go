package library

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// logIndex is what library mode learns from the audit log, so that it does
// not have to read every asset's metadata at startup. It is a cache, not
// state: Immich's markers are the truth and are read again before anything
// is done, and a log that is missing, trimmed or wrong only means that less
// is found without a full read of the library.
type logIndex struct {
	mu   sync.Mutex
	path string
	// user is the Immich user this index is for. Entries of other users
	// are left out; entries without a user (proxy and watcher conversions,
	// and library mode before several users) count for everyone.
	user string
	// offset is how far the file has been read; rest is an incomplete last
	// line, kept until it is finished.
	offset int64
	rest   []byte

	// originals maps the SHA-1 (lowercase hex) of an original to the asset
	// it was converted into.
	originals map[string]string
	// pending are copies library mode uploaded whose replacement the log
	// does not show as finished or undone.
	pending map[string]bool
	// known says the log holds anything library mode can go by: a
	// conversion, a replacement, or a full read.
	known bool
}

// eventFullRead is logged after library mode read the whole library.
const eventFullRead = "full_read"

func newLogIndex(path, user string) *logIndex {
	return &logIndex{path: path, user: user, originals: map[string]string{}, pending: map[string]bool{}}
}

// refresh reads what was added to the log since the last call.
func (x *logIndex) refresh() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	f, err := os.Open(x.path) // #nosec G304 -- the optimizer's own audit log
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < x.offset {
		// The log was replaced or truncated: start over.
		x.offset, x.rest = 0, nil
	}
	if _, err := f.Seek(x.offset, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		x.offset += int64(len(line))
		if len(line) > 0 && line[len(line)-1] == '\n' {
			x.add(append(x.rest, line...))
			x.rest = nil
		} else if len(line) > 0 {
			x.rest = append(x.rest, line...)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// add takes one log line into account. Lines it cannot read are ignored.
func (x *logIndex) add(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	var e Entry
	if json.Unmarshal(line, &e) != nil || e.User != "" && x.user != "" && e.User != x.user {
		return
	}
	sha := strings.ToLower(e.OriginalSHA1)
	switch {
	case e.Event == eventFullRead:
		x.known = true
	case e.Source == string(entity.SourceLibrary):
		switch e.Event {
		case "uploaded":
			if e.NewAsset != "" {
				x.pending[e.NewAsset], x.known = true, true
			}
		case "replaced":
			delete(x.pending, e.NewAsset)
			if sha != "" && e.NewAsset != "" {
				x.originals[sha] = e.NewAsset
			}
			x.known = true
		case "finalized", "rolled_back":
			delete(x.pending, e.NewAsset)
		}
	case e.Event == "uploaded" || e.Event == "duplicate":
		// A conversion by the proxy or the watcher.
		if sha != "" && e.Asset != "" && e.Outcome == entity.OutcomeOptimized.String() {
			x.originals[sha], x.known = e.Asset, true
		}
	}
}

// convertedFrom returns the asset an original with this SHA-1 was converted
// into, if the log knows one.
func (x *logIndex) convertedFrom(sha1Hex string) (string, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	id, ok := x.originals[strings.ToLower(sha1Hex)]
	return id, ok
}

// remember adds what a full read found in the markers.
func (x *logIndex) remember(originals map[string]string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for sha, id := range originals {
		x.originals[sha] = id
	}
}

func (x *logIndex) pendingIDs() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	ids := make([]string, 0, len(x.pending))
	for id := range x.pending {
		ids = append(ids, id)
	}
	return ids
}

func (x *logIndex) isKnown() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.known
}
