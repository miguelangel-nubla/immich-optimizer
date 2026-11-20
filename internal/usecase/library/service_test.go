package library

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

const me = "user-1"

func sha(b []byte) string {
	s := sha1.Sum(b)
	return hex.EncodeToString(s[:])
}

// fakeAsset is an asset in the fake Immich.
type fakeAsset struct {
	entity.LibraryAsset
	data     []byte
	metadata map[string]json.RawMessage
	albums   []string
	links    []string
	faces    []entity.LibraryFace
	deleted  bool // permanently
}

// fakeImmich is an in-memory Immich that behaves like the real one where
// library mode depends on it. calls records every mutating call in order.
type fakeImmich struct {
	assets map[string]*fakeAsset
	order  []string
	nextID int
	calls  []string

	// knobs
	copyDropsTags bool                // copy does not bring tags over, as when no sidecar exists
	onUpload      func(a *fakeAsset)  // tamper with a freshly uploaded asset
	beforeTrash   func(f *fakeImmich) // runs right before a Trash call is applied
	failCall      map[string]error    // method name -> error
	activity      map[string]int      // albumID/assetID -> count
	sharedAlbums  map[string]bool
	neverReady    bool
}

func newFake() *fakeImmich {
	return &fakeImmich{assets: map[string]*fakeAsset{}, failCall: map[string]error{}, activity: map[string]int{}, sharedAlbums: map[string]bool{}}
}

func (f *fakeImmich) add(name string, data []byte) *fakeAsset {
	f.nextID++
	id := fmt.Sprintf("asset-%d", f.nextID)
	a := &fakeAsset{data: data, metadata: map[string]json.RawMessage{}}
	a.ID, a.OwnerID, a.Type, a.OriginalFileName = id, me, "IMAGE", name
	a.Checksum, a.Size = sha(data), int64(len(data))
	a.Visibility, a.Ready = "timeline", true
	a.FileCreatedAt = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	a.FileModifiedAt, a.LocalDateTime = a.FileCreatedAt, a.FileCreatedAt
	a.Width, a.Height = 640, 480
	f.assets[id] = a
	f.order = append(f.order, id)
	return a
}

func (f *fakeImmich) fail(name string) error { return f.failCall[name] }

func (f *fakeImmich) get(id string) (*fakeAsset, error) {
	a := f.assets[id]
	if a == nil || a.deleted {
		return nil, fmt.Errorf("%w: %s", entity.ErrAssetNotFound, id)
	}
	return a, nil
}

func (f *fakeImmich) CurrentUserID(context.Context) (string, error) {
	return me, f.fail("CurrentUserID")
}

func (f *fakeImmich) ListAssets(_ context.Context, page int) ([]entity.LibraryAsset, int, error) {
	if err := f.fail("ListAssets"); err != nil {
		return nil, 0, err
	}
	const size = 2
	var live []entity.LibraryAsset
	for _, id := range f.order {
		if a := f.assets[id]; !a.deleted && !a.IsTrashed {
			l := a.LibraryAsset
			l.TagIDs, l.PersonIDs, l.StackID = nil, nil, "" // the listing has no tags or stack
			live = append(live, l)
		}
	}
	start := (page - 1) * size
	if start >= len(live) {
		return nil, 0, nil
	}
	end, next := start+size, page+1
	if end >= len(live) {
		end, next = len(live), 0
	}
	return live[start:end], next, nil
}

func (f *fakeImmich) GetAsset(_ context.Context, id string) (*entity.LibraryAsset, error) {
	if err := f.fail("GetAsset"); err != nil {
		return nil, err
	}
	a, err := f.get(id)
	if err != nil {
		return nil, err
	}
	c := a.LibraryAsset
	c.TagIDs = append([]string(nil), a.TagIDs...)
	c.PersonIDs = nil
	for _, face := range a.faces {
		c.PersonIDs = append(c.PersonIDs, face.PersonID)
	}
	if f.neverReady && strings.HasSuffix(a.OriginalFileName, ".jxl") {
		c.Ready = false
	}
	return &c, nil
}

func (f *fakeImmich) GetMetadata(_ context.Context, id string) ([]entity.MetadataItem, error) {
	a, err := f.get(id)
	if err != nil {
		return nil, err
	}
	var items []entity.MetadataItem
	for k, v := range a.metadata {
		items = append(items, entity.MetadataItem{Key: k, Value: v})
	}
	return items, nil
}

func (f *fakeImmich) PutMetadata(_ context.Context, id string, items []entity.MetadataItem) error {
	if err := f.fail("PutMetadata"); err != nil {
		return err
	}
	a, err := f.get(id)
	if err != nil {
		return err
	}
	f.calls = append(f.calls, "metadata:"+id)
	for _, it := range items {
		a.metadata[it.Key] = it.Value
	}
	return nil
}

func (f *fakeImmich) DownloadOriginal(_ context.Context, id, dst string) (string, error) {
	if err := f.fail("DownloadOriginal"); err != nil {
		return "", err
	}
	a, err := f.get(id)
	if err != nil {
		return "", err
	}
	return sha(a.data), os.WriteFile(dst, a.data, 0o600)
}

func (f *fakeImmich) Upload(_ context.Context, u entity.LibraryUpload) (*entity.LibraryUploadResult, error) {
	if err := f.fail("Upload"); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(u.Path)
	if err != nil {
		return nil, err
	}
	for _, id := range f.order {
		if a := f.assets[id]; !a.deleted && a.Checksum == sha(data) {
			return &entity.LibraryUploadResult{ID: id, Duplicate: true}, nil
		}
	}
	a := f.add(u.Filename, data)
	a.FileCreatedAt, a.FileModifiedAt, a.LocalDateTime = u.FileCreatedAt, u.FileModifiedAt, u.FileCreatedAt
	a.IsFavorite, a.Visibility = u.IsFavorite, u.Visibility
	for _, it := range u.Metadata {
		a.metadata[it.Key] = it.Value
	}
	f.calls = append(f.calls, "upload:"+a.ID)
	if f.onUpload != nil {
		f.onUpload(a)
	}
	return &entity.LibraryUploadResult{ID: a.ID}, nil
}

func (f *fakeImmich) CopyAsset(_ context.Context, sourceID, targetID string) error {
	if err := f.fail("CopyAsset"); err != nil {
		return err
	}
	src, err := f.get(sourceID)
	if err != nil {
		return err
	}
	dst, err := f.get(targetID)
	if err != nil {
		return err
	}
	f.calls = append(f.calls, "copy:"+targetID)
	dst.albums = append([]string(nil), src.albums...)
	dst.IsFavorite = src.IsFavorite
	if src.StackID != "" {
		dst.StackID, dst.StackPrimaryID = src.StackID, src.StackPrimaryID
	}
	// The sidecar brings description, rating and (usually) tags.
	dst.Description, dst.Rating = src.Description, src.Rating
	if !f.copyDropsTags {
		dst.TagIDs = append([]string(nil), src.TagIDs...)
	}
	return nil
}

func (f *fakeImmich) UpdateAsset(_ context.Context, id string, u entity.LibraryAssetUpdate) error {
	if err := f.fail("UpdateAsset"); err != nil {
		return err
	}
	a, err := f.get(id)
	if err != nil {
		return err
	}
	if u.Description != nil {
		a.Description = *u.Description
	}
	if u.Rating != nil {
		a.Rating = u.Rating
	}
	if u.DateTimeOriginal != nil {
		a.DateTimeOriginal = *u.DateTimeOriginal
	}
	if u.Latitude != nil {
		a.Latitude, a.Longitude = u.Latitude, u.Longitude
	}
	if u.IsFavorite != nil {
		a.IsFavorite = *u.IsFavorite
	}
	if u.Visibility != nil {
		a.Visibility = *u.Visibility
	}
	return nil
}

func (f *fakeImmich) Albums(_ context.Context, assetID string) ([]entity.LibraryAlbum, error) {
	a, err := f.get(assetID)
	if err != nil {
		return nil, err
	}
	var out []entity.LibraryAlbum
	for _, id := range a.albums {
		out = append(out, entity.LibraryAlbum{ID: id, Name: id, Shared: f.sharedAlbums[id]})
	}
	return out, nil
}

func (f *fakeImmich) ActivityCount(_ context.Context, albumID, assetID string) (int, error) {
	return f.activity[albumID+"/"+assetID], nil
}

func (f *fakeImmich) TagAsset(_ context.Context, assetID string, tagIDs []string) error {
	a, err := f.get(assetID)
	if err != nil {
		return err
	}
	a.TagIDs = append(a.TagIDs, tagIDs...)
	return nil
}

func (f *fakeImmich) Faces(_ context.Context, assetID string) ([]entity.LibraryFace, error) {
	a, err := f.get(assetID)
	if err != nil {
		return nil, err
	}
	return a.faces, nil
}

func (f *fakeImmich) CreateFace(_ context.Context, assetID string, face entity.LibraryFace) error {
	if err := f.fail("CreateFace"); err != nil {
		return err
	}
	a, err := f.get(assetID)
	if err != nil {
		return err
	}
	a.faces = append(a.faces, face)
	return nil
}

func (f *fakeImmich) SharedLinkIDs(_ context.Context, assetID string) ([]string, error) {
	a, err := f.get(assetID)
	if err != nil {
		return nil, err
	}
	return a.links, nil
}

func (f *fakeImmich) AddToSharedLink(_ context.Context, linkID, assetID string) error {
	a, err := f.get(assetID)
	if err != nil {
		return err
	}
	a.links = append(a.links, linkID)
	return nil
}

func (f *fakeImmich) SetStackPrimary(_ context.Context, stackID, assetID string) error {
	for _, a := range f.assets {
		if a.StackID == stackID {
			a.StackPrimaryID = assetID
		}
	}
	return nil
}

func (f *fakeImmich) Trash(_ context.Context, ids []string) error {
	if f.beforeTrash != nil {
		f.beforeTrash(f)
	}
	if err := f.fail("Trash"); err != nil {
		return err
	}
	for _, id := range ids {
		a, err := f.get(id)
		if err != nil {
			return err
		}
		a.IsTrashed = true
		f.calls = append(f.calls, "trash:"+id)
	}
	return nil
}

func (f *fakeImmich) Delete(_ context.Context, ids []string) error {
	if err := f.fail("Delete"); err != nil {
		return err
	}
	for _, id := range ids {
		a, err := f.get(id)
		if err != nil {
			return err
		}
		a.deleted = true
		f.calls = append(f.calls, "delete:"+id)
	}
	return nil
}

// fakeConv "converts" by keeping the first half of the file.
type fakeConv struct {
	noGain     bool
	convertErr error
	verifyErr  error
	motion     string
	verified   int
}

func (c *fakeConv) Convert(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
	if c.convertErr != nil {
		return nil, c.convertErr
	}
	if !in.SkipUndone {
		return nil, errors.New("library mode must not save originals to undone")
	}
	data, err := os.ReadFile(in.Path)
	if err != nil {
		return nil, err
	}
	if c.noGain {
		return &entity.ConvertResult{Outcome: entity.OutcomeUnchanged, DeliverPath: in.Path, DeliverName: in.Filename,
			OriginalSize: int64(len(data)), OutputSize: int64(len(data)), Release: func() {}}, nil
	}
	out := filepath.Join(filepath.Dir(in.Path), "converted.jxl")
	if err := os.WriteFile(out, append([]byte("JXL:"), data[:len(data)/2]...), 0o600); err != nil {
		return nil, err
	}
	st, _ := os.Stat(out)
	return &entity.ConvertResult{
		Outcome: entity.OutcomeOptimized, DeliverPath: out,
		DeliverName:  strings.TrimSuffix(in.Filename, filepath.Ext(in.Filename)) + ".jxl",
		OriginalSize: int64(len(data)), OutputSize: st.Size(), Release: func() {},
		Marker: &entity.Marker{V: 1, SHA1: in.SHA1, Size: int64(len(data)), Task: "jpeg-xl", Source: string(in.Source), MotionPhotos: c.motion},
	}, nil
}

func (c *fakeConv) SaveUndone(entity.ConvertInput, entity.Failure) (*entity.UndoneRecord, error) {
	return nil, errors.New("not used")
}

func (c *fakeConv) CheckOutput(context.Context, entity.ConvertInput, *entity.ConvertResult) error {
	c.verified++
	return c.verifyErr
}

func (c *fakeConv) Converts(filename string) (bool, string) {
	if strings.HasSuffix(strings.ToLower(filename), ".jpg") {
		return true, ""
	}
	return false, "no task handles this file type"
}

type harness struct {
	t       *testing.T
	im      *fakeImmich
	conv    *fakeConv
	logPath string
	cfg     Config
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, im: newFake(), conv: &fakeConv{}, logPath: filepath.Join(t.TempDir(), "log", "library.jsonl"),
		cfg: Config{ReadyTimeout: 50 * time.Millisecond, PollInterval: time.Millisecond, Version: "test"}}
}

func (h *harness) run() (Summary, error) {
	h.t.Helper()
	j, err := audit.Open(h.logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer j.Close()
	return New(h.cfg, h.im, h.conv, j, nil).Run(context.Background())
}

// events returns the logged event names, in order.
func (h *harness) events() []string {
	h.t.Helper()
	f, err := os.Open(h.logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			h.t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		out = append(out, e.Event)
	}
	return out
}

func (h *harness) marker(id string) *entity.Marker {
	a := h.im.assets[id]
	raw, ok := a.metadata[entity.MarkerKey]
	if !ok {
		return nil
	}
	var m entity.Marker
	if err := json.Unmarshal(raw, &m); err != nil {
		h.t.Fatal(err)
	}
	return &m
}

func index(list []string, want string) int {
	for i, s := range list {
		if s == want {
			return i
		}
	}
	return -1
}

func photo(n int) []byte { return []byte(strings.Repeat(fmt.Sprintf("photo-%d ", n), 50)) }

func richAsset(h *harness) *fakeAsset {
	a := h.im.add("IMG_1.jpg", photo(1))
	rating, lat, lon := 4.0, 40.4, -3.7
	a.IsFavorite, a.Visibility = true, "archive"
	a.Description, a.Rating, a.Latitude, a.Longitude = "holiday", &rating, &lat, &lon
	a.DateTimeOriginal, a.TimeZone, a.Model = "2020-01-02T03:04:05+00:00", "UTC", "Cam"
	a.TagIDs = []string{"tag-1", "tag-2"}
	a.albums = []string{"album-1"}
	a.links = []string{"link-1"}
	a.faces = []entity.LibraryFace{{PersonID: "alice", X1: 1, Y1: 2, X2: 30, Y2: 40, ImageWidth: 640, ImageHeight: 480}}
	a.metadata["mobile-app"] = json.RawMessage(`{"iCloudId":"X:1"}`)
	a.StackID, a.StackPrimaryID = "stack-1", a.ID
	// What Immich reads from the converted file itself.
	h.im.onUpload = func(n *fakeAsset) {
		n.DateTimeOriginal, n.TimeZone, n.Model = a.DateTimeOriginal, a.TimeZone, a.Model
		n.Latitude, n.Longitude = a.Latitude, a.Longitude
	}
	return a
}

func TestReplaceCarriesEverythingOverThenTrashes(t *testing.T) {
	h := newHarness(t)
	h.im.copyDropsTags = true
	old := richAsset(h)

	sum, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Replaced != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if !old.IsTrashed || old.deleted {
		t.Fatal("the original must be in the trash, not deleted")
	}
	var n *fakeAsset
	for _, a := range h.im.assets {
		if a.ID != old.ID {
			n = a
		}
	}
	if n == nil || n.deleted || n.IsTrashed {
		t.Fatal("no live converted asset")
	}
	if n.OriginalFileName != "IMG_1.jpg"[:5]+".jxl" || !n.IsFavorite || n.Visibility != "archive" || n.Description != "holiday" ||
		*n.Rating != 4 || len(n.TagIDs) != 2 || len(n.albums) != 1 || len(n.links) != 1 || len(n.faces) != 1 ||
		n.StackPrimaryID != n.ID || !n.FileCreatedAt.Equal(old.FileCreatedAt) {
		t.Errorf("converted asset is missing something: %+v albums=%v links=%v faces=%v", n.LibraryAsset, n.albums, n.links, n.faces)
	}
	if string(n.metadata["mobile-app"]) != `{"iCloudId":"X:1"}` {
		t.Error("the original's own metadata item was not carried over")
	}
	m := h.marker(n.ID)
	if m == nil || m.Replaces != old.ID || m.State != "" || m.SHA1 != old.Checksum || m.Source != "library" {
		t.Errorf("final marker = %+v", m)
	}
	if h.conv.verified != 1 {
		t.Errorf("output check ran %d times, want 1", h.conv.verified)
	}

	// Order: upload, copy, then trash, and only then the final marker.
	calls := h.im.calls
	up, cp, tr := index(calls, "upload:"+n.ID), index(calls, "copy:"+n.ID), index(calls, "trash:"+old.ID)
	if up < 0 || !(up < cp && cp < tr) || index(calls, "metadata:"+n.ID) < tr {
		t.Errorf("wrong order of changes: %v", calls)
	}
	ev := h.events()
	for _, want := range []string{"run_started", "converted", "uploaded", "verified", "replaced", "run_finished"} {
		if index(ev, want) < 0 {
			t.Errorf("log has no %q event: %v", want, ev)
		}
	}
	if index(ev, "verified") > index(ev, "replaced") {
		t.Errorf("log order wrong: %v", ev)
	}

	// A second run finds nothing to do.
	h.im.calls = nil
	sum, err = h.run()
	if err != nil || sum.Replaced != 0 || sum.Skipped != 1 || len(h.im.calls) != 0 {
		t.Errorf("second run: %+v %v calls=%v", sum, err, h.im.calls)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.cfg.DryRun = true
	old := richAsset(h)
	sum, err := h.run()
	if err != nil || sum.WouldReplace != 1 || sum.Replaced != 0 {
		t.Fatalf("summary = %+v, err %v", sum, err)
	}
	if len(h.im.calls) != 0 || old.IsTrashed || len(h.im.assets) != 1 || h.marker(old.ID) != nil {
		t.Errorf("dry run changed Immich: %v", h.im.calls)
	}
	if index(h.events(), "would_convert") < 0 {
		t.Error("dry run did not log what it would do")
	}
}

// Any mismatch after the upload removes the new asset and leaves the original
// exactly as it was.
func TestMismatchRollsBack(t *testing.T) {
	cases := map[string]func(h *harness){
		"metadata read from file differs": func(h *harness) {
			h.im.onUpload = func(n *fakeAsset) { n.Model = "Other" }
		},
		"face cannot be recreated": func(h *harness) {
			h.im.failCall["CreateFace"] = errors.New("boom")
		},
		"copy fails": func(h *harness) {
			h.im.failCall["CopyAsset"] = errors.New("boom")
		},
		"immich never finishes reading it": func(h *harness) {
			h.im.neverReady = true
		},
		"immich stored other bytes": func(h *harness) {
			prev := h.im.onUpload
			h.im.onUpload = func(n *fakeAsset) { prev(n); n.Checksum = sha([]byte("corrupt")) }
		},
		"original edited meanwhile": func(h *harness) {
			h.im.beforeTrash = nil
			prev := h.im.onUpload
			h.im.onUpload = func(n *fakeAsset) {
				prev(n)
				// The user edits the original after the snapshot was taken:
				// the new asset matches the snapshot, not the edit.
				for _, a := range h.im.assets {
					if a.ID != n.ID {
						a.IsFavorite = !a.IsFavorite
					}
				}
			}
		},
		"trash fails": func(h *harness) {
			h.im.failCall["Trash"] = errors.New("boom")
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			old := richAsset(h)
			setup(h)
			sum, err := h.run()
			if err != nil {
				t.Fatal(err)
			}
			if sum.Failed != 1 || sum.Replaced != 0 {
				t.Fatalf("summary = %+v", sum)
			}
			if old.IsTrashed || old.deleted {
				t.Fatal("the original was touched")
			}
			if h.marker(old.ID) != nil {
				t.Error("a marker was written on the original")
			}
			for _, a := range h.im.assets {
				if a.ID != old.ID && !a.deleted {
					t.Errorf("the new asset %s was left behind", a.ID)
				}
			}
			ev := h.events()
			if index(ev, "rolled_back") < 0 || index(ev, "failed") < 0 || index(ev, "replaced") >= 0 {
				t.Errorf("log = %v", ev)
			}
		})
	}
}

// Failures before the upload change nothing at all.
func TestFailuresBeforeUploadChangeNothing(t *testing.T) {
	cases := map[string]func(h *harness){
		"conversion fails":        func(h *harness) { h.conv.convertErr = errors.New("encoder crashed") },
		"output is not valid":     func(h *harness) { h.conv.verifyErr = errors.New("single flat colour") },
		"download does not match": func(h *harness) { h.im.assets["asset-1"].Checksum = sha([]byte("something else")) },
		"upload fails":            func(h *harness) { h.im.failCall["Upload"] = errors.New("quota") },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			old := richAsset(h)
			setup(h)
			sum, err := h.run()
			if err != nil || sum.Failed != 1 {
				t.Fatalf("summary = %+v, err %v", sum, err)
			}
			if len(h.im.calls) != 0 || old.IsTrashed || len(h.im.assets) != 1 {
				t.Errorf("Immich was changed: %v", h.im.calls)
			}
			if index(h.events(), "failed") < 0 {
				t.Error("failure not logged")
			}
		})
	}
}

func TestNoGainKeepsOriginalAndRemembers(t *testing.T) {
	h := newHarness(t)
	h.conv.noGain = true
	old := richAsset(h)
	sum, err := h.run()
	if err != nil || sum.Kept != 1 || sum.Replaced != 0 {
		t.Fatalf("summary = %+v, err %v", sum, err)
	}
	m := h.marker(old.ID)
	if old.IsTrashed || m == nil || !m.Kept || m.Note == "" {
		t.Errorf("original trashed=%v marker=%+v", old.IsTrashed, m)
	}
	// Next run does not download it again.
	h.im.failCall["DownloadOriginal"] = errors.New("must not be called")
	if sum, err := h.run(); err != nil || sum.Failed != 0 || sum.Skipped != 1 {
		t.Errorf("second run: %+v %v", sum, err)
	}
}

func TestMotionPhotoIsKept(t *testing.T) {
	h := newHarness(t)
	h.conv.motion = entity.MotionPhotosDropVideo
	old := richAsset(h)
	sum, err := h.run()
	if err != nil || sum.Kept != 1 || old.IsTrashed || len(h.im.assets) != 1 {
		t.Fatalf("summary = %+v, err %v, trashed %v", sum, err, old.IsTrashed)
	}
}

func TestSkips(t *testing.T) {
	h := newHarness(t)
	mk := func(name string, mod func(a *fakeAsset)) {
		a := h.im.add(name, photo(len(h.im.assets)+10))
		mod(a)
	}
	mk("other-user.jpg", func(a *fakeAsset) { a.OwnerID = "user-2" })
	mk("external.jpg", func(a *fakeAsset) { a.LibraryID = "lib-1" })
	mk("offline.jpg", func(a *fakeAsset) { a.IsOffline = true })
	mk("hidden.jpg", func(a *fakeAsset) { a.Visibility = "hidden" })
	mk("locked.jpg", func(a *fakeAsset) { a.Visibility = "locked" })
	mk("live.jpg", func(a *fakeAsset) { a.LivePhotoVideoID = "video-1" })
	mk("edited.jpg", func(a *fakeAsset) { a.IsEdited = true })
	mk("clip.mp4", func(a *fakeAsset) {})
	mk("not-ready.jpg", func(a *fakeAsset) { a.Ready = false })
	mk("commented.jpg", func(a *fakeAsset) {
		a.albums = []string{"shared-1"}
		h.im.sharedAlbums["shared-1"] = true
		h.im.activity["shared-1/"+a.ID] = 2
	})
	mk("done.jpg", func(a *fakeAsset) { a.metadata[entity.MarkerKey] = json.RawMessage(`{"v":1,"task":"jpeg-xl"}`) })
	mk("new.jpg", func(a *fakeAsset) { a.FileCreatedAt = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) })
	h.cfg.TakenBefore = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	sum, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Skipped != 12 || sum.Replaced != 0 || sum.Failed != 0 || len(h.im.calls) != 0 {
		t.Errorf("summary = %+v, calls %v", sum, h.im.calls)
	}
}

func TestSharedAlbumWithoutActivityIsConverted(t *testing.T) {
	h := newHarness(t)
	a := richAsset(h)
	a.albums = []string{"shared-1"}
	h.im.sharedAlbums["shared-1"] = true
	if sum, err := h.run(); err != nil || sum.Replaced != 1 {
		t.Fatalf("summary = %+v, err %v", sum, err)
	}
}

func TestLimit(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.im.add(fmt.Sprintf("IMG_%d.jpg", i), photo(i))
	}
	h.cfg.Limit = 2
	sum, err := h.run()
	if err != nil || sum.Replaced != 2 {
		t.Fatalf("summary = %+v, err %v", sum, err)
	}
	trashed := 0
	for _, a := range h.im.assets {
		if a.IsTrashed {
			trashed++
		}
	}
	if trashed != 2 {
		t.Errorf("%d originals trashed, want 2", trashed)
	}
}

// pendingCopy puts the fake in the state an interrupted run leaves behind:
// the converted copy exists and is marked pending.
func pendingCopy(h *harness, old *fakeAsset) *fakeAsset {
	data := append([]byte("JXL:"), old.data[:len(old.data)/2]...)
	n := h.im.add("IMG_1.jxl", data)
	m, _ := json.Marshal(entity.Marker{V: 1, SHA1: old.Checksum, Task: "jpeg-xl", Source: "library", Replaces: old.ID, State: entity.MarkerStatePending})
	n.metadata[entity.MarkerKey] = m
	return n
}

func TestInterruptedBeforeTrashIsCompletedByNextRun(t *testing.T) {
	h := newHarness(t)
	old := richAsset(h)
	n := pendingCopy(h, old)
	prev := h.im.onUpload
	prev(n) // what Immich read from the file

	sum, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	// The original is reached first: its conversion gives the same bytes,
	// Immich answers "duplicate", and the unfinished copy is completed.
	if sum.Replaced != 1 || !old.IsTrashed || n.deleted {
		t.Fatalf("summary = %+v, old trashed %v, copy deleted %v", sum, old.IsTrashed, n.deleted)
	}
	if m := h.marker(n.ID); m == nil || m.State != "" {
		t.Errorf("marker still pending: %+v", m)
	}
	live := 0
	for _, a := range h.im.assets {
		if !a.deleted && !a.IsTrashed {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d live assets, want 1", live)
	}
}

func TestInterruptedAfterTrashIsFinalized(t *testing.T) {
	h := newHarness(t)
	old := richAsset(h)
	n := pendingCopy(h, old)
	old.IsTrashed = true

	sum, err := h.run()
	if err != nil || sum.Finalized != 1 {
		t.Fatalf("summary = %+v, err %v", sum, err)
	}
	if m := h.marker(n.ID); m == nil || m.State != "" || n.deleted {
		t.Errorf("marker = %+v, deleted %v", m, n.deleted)
	}
	if index(h.events(), "finalized") < 0 {
		t.Error("not logged")
	}
}

func TestOrphanedPendingCopyIsRemovedWhenOriginalStillLive(t *testing.T) {
	h := newHarness(t)
	// The copy is listed before its original and the original can no longer
	// be converted to the same bytes: the copy must go, the original stays.
	old := h.im.add("IMG_1.jpg", photo(1))
	h.im.order = nil
	n := pendingCopy(h, old)
	h.im.order = []string{n.ID, old.ID}
	h.conv.convertErr = errors.New("encoder changed")

	sum, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if !n.deleted || old.IsTrashed || old.deleted || sum.RolledBack != 1 {
		t.Errorf("copy deleted %v, original trashed %v, summary %+v", n.deleted, old.IsTrashed, sum)
	}
}

func TestDuplicateOfUnrelatedAssetIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	old := h.im.add("IMG_1.jpg", photo(1))
	// The user already has the converted bytes as an ordinary asset.
	other := h.im.add("mine.jxl", append([]byte("JXL:"), old.data[:len(old.data)/2]...))

	sum, err := h.run()
	if err != nil {
		t.Fatal(err)
	}
	if old.IsTrashed || other.deleted || other.IsTrashed || sum.Replaced != 0 || sum.Failed != 0 {
		t.Errorf("summary %+v, old trashed %v, other deleted %v", sum, old.IsTrashed, other.deleted)
	}
	for _, c := range h.im.calls {
		if strings.HasPrefix(c, "delete:") || strings.HasPrefix(c, "trash:") || strings.HasPrefix(c, "copy:") {
			t.Errorf("unexpected change: %v", h.im.calls)
		}
	}
}

func TestUnwritableLogStopsTheRunBeforeAnyChange(t *testing.T) {
	h := newHarness(t)
	old := richAsset(h)
	j, err := audit.Open(h.logPath)
	if err != nil {
		t.Fatal(err)
	}
	j.Close() // every write now fails
	_, err = New(h.cfg, h.im, h.conv, j, nil).Run(context.Background())
	if err == nil {
		t.Fatal("expected the run to stop")
	}
	if len(h.im.calls) != 0 || old.IsTrashed {
		t.Errorf("Immich was changed without a log: %v", h.im.calls)
	}
}

func TestListingErrorStopsTheRun(t *testing.T) {
	h := newHarness(t)
	richAsset(h)
	h.im.failCall["ListAssets"] = errors.New("immich down")
	if _, err := h.run(); err == nil {
		t.Fatal("expected an error")
	}
	if len(h.im.calls) != 0 {
		t.Errorf("changes made: %v", h.im.calls)
	}
}

// A task may crop, resize or rotate: a different picture shape is not a
// mismatch, and a re-encoded video may be slightly shorter.
func TestReplacementMayChangeShape(t *testing.T) {
	h := newHarness(t)
	old := richAsset(h)
	old.Orientation, old.Duration = "6", "2000"
	prev := h.im.onUpload
	h.im.onUpload = func(n *fakeAsset) { prev(n); n.Width, n.Height, n.Orientation, n.Duration = 320, 200, "1", "1980" }
	if sum, err := h.run(); err != nil || sum.Replaced != 1 {
		t.Fatalf("summary = %+v, err %v", sum, err)
	}
	if !similarDuration("2000", "2400") || similarDuration("2000", "9000") || !similarDuration("", "") || similarDuration("0:00:02", "0:00:03") {
		t.Error("similarDuration")
	}
}
