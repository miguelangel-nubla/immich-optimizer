package library

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

func init() {
	settleDelay = 10 * time.Millisecond
	retryDelay = 10 * time.Millisecond
}

// fakeEvents stands in for Immich's websocket.
type fakeEvents struct {
	mu sync.Mutex
	h  *port.LibraryEventHandlers
}

func (e *fakeEvents) Listen(ctx context.Context, h port.LibraryEventHandlers) {
	e.mu.Lock()
	e.h = &h
	e.mu.Unlock()
	h.Connected()
	<-ctx.Done()
}

func (e *fakeEvents) handlers(t *testing.T) port.LibraryEventHandlers {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		h := e.h
		e.mu.Unlock()
		if h != nil {
			return *h
		}
		if time.Now().After(deadline) {
			t.Fatal("Serve never started listening")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// addLive adds an asset while Serve may be reading the fake.
func (f *fakeImmich) addLive(name string, data []byte) *fakeAsset {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(name, data)
}

// serve runs Serve until the test ends.
func (h *harness) serve() *fakeEvents {
	h.t.Helper()
	j, err := audit.Open(h.logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ev := &fakeEvents{}
	done := make(chan error, 1)
	go func() { done <- New(h.cfg, h.im, h.conv, j, nil).Serve(ctx, ev) }()
	h.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			h.t.Errorf("Serve returned %v", err)
		}
		j.Close()
	})
	return ev
}

// waitFor polls the fake under its lock until check passes.
func (h *harness) waitFor(what string, check func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.im.mu.Lock()
		ok := check()
		h.im.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// eventCount counts an event in the audit log.
func (h *harness) eventCount(name string) int {
	n := 0
	for _, e := range h.events() {
		if e == name {
			n++
		}
	}
	return n
}

func TestServeReadsTheWholeLibraryOnlyWhenTheLogKnowsNothing(t *testing.T) {
	h := tagged(t)
	h.im.add("IMG_1.jpg", photo(1))
	h.serve()
	h.waitFor("the full read", func() bool { return h.im.lists == 1 })
	h.waitFor("the full read to be logged", func() bool {
		h.im.mu.Unlock()
		defer h.im.mu.Lock()
		return h.eventCount(eventFullRead) == 1
	})

	// Started again: the log now knows the library was read.
	h2 := &harness{t: t, im: h.im, conv: h.conv, logPath: h.logPath, cfg: h.cfg}
	h2.serve()
	time.Sleep(100 * time.Millisecond)
	h.im.mu.Lock()
	lists := h.im.lists
	h.im.mu.Unlock()
	if lists != 1 {
		t.Errorf("the library was listed %d times, want once", lists)
	}
}

func TestServeConvertsWhatIsTagged(t *testing.T) {
	h := tagged(t)
	writeLog(t, h.logPath, `{"source":"library","event":"full_read"}`+"\n")
	a := h.im.add("IMG_1.jpg", photo(1))
	ev := h.serve()
	hs := ev.handlers(t)

	h.im.mu.Lock()
	h.im.tags[TagConvert] = "tag-convert"
	a.TagIDs = append(a.TagIDs, "tag-convert")
	h.im.mu.Unlock()
	hs.AssetUpdated(a.ID, []string{TagConvert})
	h.waitFor("the tagged photo to be converted", func() bool { return a.IsTrashed })

	// An update that is not about the optimizer's tags starts nothing.
	b := h.im.addLive("IMG_2.jpg", photo(2))
	hs.AssetUpdated(b.ID, []string{"holiday"})
	time.Sleep(100 * time.Millisecond)
	h.im.mu.Lock()
	defer h.im.mu.Unlock()
	if b.IsTrashed || h.im.lists != 0 {
		t.Errorf("an unrelated update did something: trashed %v, listings %d", b.IsTrashed, h.im.lists)
	}
}

// A tag set while nothing was listening is found when the connection is
// made again.
func TestServeLooksForTagsOnEveryConnection(t *testing.T) {
	h := tagged(t)
	writeLog(t, h.logPath, `{"source":"library","event":"full_read"}`+"\n")
	ev := h.serve()
	hs := ev.handlers(t)
	a := h.im.addLive("IMG_1.jpg", photo(1))
	h.im.mu.Lock()
	h.im.tags[TagConvert] = "tag-convert"
	a.TagIDs = append(a.TagIDs, "tag-convert")
	h.im.mu.Unlock()
	hs.Connected()
	h.waitFor("the photo tagged while disconnected", func() bool { return a.IsTrashed })
}

// An interrupted replacement is found through the log, without reading the
// library.
func TestServeFinishesAnInterruptedReplacementFromTheLog(t *testing.T) {
	h := tagged(t)
	old := h.im.add("IMG_1.jpg", photo(1))
	old.IsTrashed = true
	n := pendingCopy(h, old)
	writeLog(t, h.logPath, `{"source":"library","event":"uploaded","asset":"`+old.ID+`","new_asset":"`+n.ID+`"}`+"\n")
	h.serve()
	h.waitFor("the unfinished copy to be completed", func() bool {
		var m entity.Marker
		return json.Unmarshal(n.metadata[entity.MarkerKey], &m) == nil && m.State == ""
	})
	h.im.mu.Lock()
	defer h.im.mu.Unlock()
	if h.im.lists != 0 {
		t.Errorf("the library was listed %d times", h.im.lists)
	}
}

func b64(hexSHA string) string {
	raw, _ := hex.DecodeString(hexSHA)
	return base64.StdEncoding.EncodeToString(raw)
}

// An upload of a file the log knows as a converted original is a second
// copy, and goes to the trash at once.
func TestServeTrashesASecondCopyWhenItIsUploaded(t *testing.T) {
	h := newHarness(t)
	data := photo(1)
	old, conv := convertOne(t, h, data) // its log has the conversion
	h.im.convertAll = false
	old.deleted = true
	ev := h.serve()
	hs := ev.handlers(t)

	again := h.im.addLive("IMG_1.jpg", data)
	hs.Uploaded(again.ID, b64(again.Checksum))
	h.waitFor("the second copy to go to the trash", func() bool { return again.IsTrashed })
	h.im.mu.Lock()
	trashed := conv.IsTrashed
	h.im.mu.Unlock()
	if trashed {
		t.Error("the converted asset was trashed")
	}

	// Anything else that is uploaded is left alone, and costs no reads.
	other := h.im.addLive("IMG_2.jpg", photo(2))
	hs.Uploaded(other.ID, b64(other.Checksum))
	hs.Uploaded(other.ID, "not base64!")
	time.Sleep(100 * time.Millisecond)
	h.im.mu.Lock()
	defer h.im.mu.Unlock()
	if other.IsTrashed {
		t.Error("an unrelated upload was trashed")
	}
}

// Started while Immich cannot be reached, it tries again.
func TestServeRetriesUntilImmichAnswers(t *testing.T) {
	h := tagged(t)
	writeLog(t, h.logPath, `{"source":"library","event":"full_read"}`+"\n")
	h.im.failCall["CurrentUserID"] = errRefused
	ev := h.serve()
	time.Sleep(50 * time.Millisecond)
	h.im.mu.Lock()
	delete(h.im.failCall, "CurrentUserID")
	h.im.mu.Unlock()
	// Serve listens for events only once it has started.
	ev.handlers(t)
}

var errRefused = &refusedErr{}

type refusedErr struct{}

func (*refusedErr) Error() string { return "connection refused" }

// Library entries say whose they are; a file that is another user's
// converted original is converted like any other.
func TestServeWithAnotherUsersConversionInTheLog(t *testing.T) {
	h := tagged(t)
	data := photo(1)
	sum := sha([]byte(data))
	writeLog(t, h.logPath, `{"source":"library","user":"someone-else","event":"replaced","asset":"x","new_asset":"their-copy","original_sha1":"`+sum+`"}`+"\n"+
		`{"source":"proxy","event":"uploaded","asset":"not-mine-either","outcome":"optimized","original_sha1":"`+sum+`"}`+"\n")
	a := h.im.add("IMG_1.jpg", data)
	h.im.tag(a, TagConvert)
	h.serve()
	h.waitFor("the photo to be converted", func() bool { return a.IsTrashed })
	h.waitFor("the replacement to be logged with its user", func() bool {
		h.im.mu.Unlock()
		defer h.im.mu.Lock()
		for _, e := range h.entries() {
			if e.Event == "replaced" && e.Asset == a.ID {
				return e.User == me
			}
		}
		return false
	})
}
