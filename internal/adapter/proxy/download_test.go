package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// fakeProcessor "converts" by writing out.jpg with fixed content.
type fakeProcessor struct {
	err   error
	calls atomic.Int32
	got   string
}

func (p *fakeProcessor) Process(_ context.Context, path string, _ []entity.Task) (*entity.ProcessResult, error) {
	p.calls.Add(1)
	p.got = filepath.Base(path)
	if p.err != nil {
		return nil, p.err
	}
	dir, err := os.MkdirTemp("", "fake-download-*")
	if err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "original.jpg")
	if err := os.WriteFile(out, []byte("0123456789-jpeg"), 0o600); err != nil {
		return nil, err
	}
	return &entity.ProcessResult{ProcessedFilePath: out, ProcessedFilename: "original.jpg", ProcessedSize: 15, OriginalSize: 3,
		Task: "jxl-to-jpeg", Cleanup: func() { _ = os.RemoveAll(dir) }}, nil
}

func TestDownloadsAreConvertedOnlyWhereATaskSaysSo(t *testing.T) {
	const jxl = "2f908a71-185e-4fb6-960c-278a8d87e66b"
	const heic = "3f908a71-185e-4fb6-960c-278a8d87e66b"
	const edited = "4f908a71-185e-4fb6-960c-278a8d87e66b"
	var originals atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"no"}`)
			return
		}
		id := strings.Split(r.URL.Path, "/")[3]
		name := map[string]string{jxl: "IMG 1.JXL", heic: "IMG_2.heic", edited: "IMG_3.jxl"}[id]
		switch {
		case strings.HasSuffix(r.URL.Path, "/original"):
			originals.Add(1)
			w.Header().Set("Content-Type", "image/jxl")
			w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
			w.Header().Set("Cache-Control", "private, max-age=86400")
			_, _ = io.WriteString(w, "stored:"+r.URL.RawQuery)
		default:
			_, _ = io.WriteString(w, `{"originalFileName":"`+name+`","isEdited":`+strconv.FormatBool(id == edited)+`}`)
		}
	}))
	defer upstream.Close()

	proc := &fakeProcessor{}
	tasks := []entity.Task{{Name: "jxl-to-jpeg", Extensions: []string{"jxl"}, Command: "x"}}
	server := newTestServer(t, upstream.URL, &fakeConverter{}, func(c *Config) { c.Downloads, c.Processor = tasks, proc })
	get := func(path string, header map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Api-Key", "k")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/api/assets/"+jxl+"/original", nil)
	if rec.Code != 200 || rec.Body.String() != "0123456789-jpeg" || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("converted download: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	if d := rec.Header().Get("Content-Disposition"); d != `inline; filename="IMG 1.jpg"` {
		t.Errorf("Content-Disposition = %q", d)
	}
	if rec.Header().Get("Cache-Control") != "private, max-age=86400" || proc.got != "original.JXL" {
		t.Errorf("Cache-Control %q, converted from %q", rec.Header().Get("Cache-Control"), proc.got)
	}
	// A part is a part of the converted file, and of the same one: a player
	// asking for a file in pieces does not have it fetched and converted for
	// every piece.
	if rec = get("/api/assets/"+jxl+"/original", map[string]string{"Range": "bytes=2-4"}); rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Errorf("range: %d %q", rec.Code, rec.Body.String())
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rec := get("/api/assets/"+jxl+"/original", map[string]string{"Range": "bytes=5-6"}); rec.Body.String() != "56" {
				t.Errorf("range: %d %q", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	if originals.Load() != 1 || proc.calls.Load() != 1 {
		t.Errorf("fetched %d times and converted %d times for one file", originals.Load(), proc.calls.Load())
	}
	// Someone else's request for the same asset is their own.
	if rec = get("/api/assets/"+jxl+"/original?key=shared", nil); rec.Code != 200 || originals.Load() != 2 {
		t.Errorf("another caller: %d, fetched %d times", rec.Code, originals.Load())
	}

	// The apps and the web always ask for the edited version. An asset that
	// was never edited in Immich is its original, and is converted.
	if rec = get("/api/assets/"+jxl+"/original?edited=true", nil); rec.Body.String() != "0123456789-jpeg" {
		t.Errorf("a download asking for the edited version of an unedited asset was not converted: %q", rec.Body.String())
	}

	// Not converted: another type, an asset that was edited in Immich, and
	// anything that is not the download of an original.
	calls := proc.calls.Load()
	for _, path := range []string{"/api/assets/" + heic + "/original", "/api/assets/" + edited + "/original?edited=true", "/api/assets/" + jxl + "/thumbnail"} {
		if get(path, nil); proc.calls.Load() != calls {
			t.Errorf("%s was converted", path)
		}
	}
	if rec = get("/api/assets/"+edited+"/original?edited=true", nil); rec.Body.String() != "stored:edited=true" {
		t.Errorf("the edited version did not pass through: %q", rec.Body.String())
	}

	// Immich's refusal is passed on, and nothing is converted for it.
	req := httptest.NewRequest(http.MethodGet, "/api/assets/"+jxl+"/original", nil)
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || proc.calls.Load() != calls {
		t.Errorf("without credentials: %d", rec.Code)
	}

	// A conversion that fails still gives the person the stored file.
	server.downloads.clear()
	proc.err = errors.New("decoder crashed")
	rec = get("/api/assets/"+jxl+"/original", nil)
	if rec.Code != 200 || rec.Body.String() != "stored:" || rec.Header().Get("Content-Type") != "image/jxl" ||
		!strings.Contains(rec.Header().Get("Content-Disposition"), "IMG 1.JXL") {
		t.Errorf("failed conversion: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}

	// Without download tasks the proxy does not look at downloads at all.
	plain := newTestServer(t, upstream.URL, &fakeConverter{})
	req = httptest.NewRequest(http.MethodGet, "/api/assets/"+jxl+"/original", nil)
	req.Header.Set("X-Api-Key", "k")
	rec = httptest.NewRecorder()
	before := originals.Load()
	plain.ServeHTTP(rec, req)
	if rec.Body.String() != "stored:" || originals.Load() != before+1 {
		t.Errorf("without tasks: %q", rec.Body.String())
	}
}

// With downloads passed through, the stored file is sent as it is, although
// a download task handles it.
func TestDownloadsPassedThroughWhenAskedTo(t *testing.T) {
	const jxl = "2f908a71-185e-4fb6-960c-278a8d87e66b"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/original") {
			w.Header().Set("Content-Type", "image/jxl")
			_, _ = io.WriteString(w, "stored")
			return
		}
		_, _ = io.WriteString(w, `{"originalFileName":"IMG_1.jxl","isEdited":false}`)
	}))
	defer upstream.Close()
	proc := &fakeProcessor{}
	tasks := []entity.Task{{Name: "jxl-to-jpeg", Extensions: []string{"jxl"}, Command: "x"}}
	server := newTestServer(t, upstream.URL, &fakeConverter{}, func(c *Config) {
		c.Downloads, c.Processor, c.PassDownloads = tasks, proc, true
	})
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/assets/"+jxl+"/original?edited=true", nil))
	if rec.Body.String() != "stored" || rec.Header().Get("Content-Type") != "image/jxl" || proc.calls.Load() != 0 {
		t.Fatalf("got %q (%s), %d conversions", rec.Body.String(), rec.Header().Get("Content-Type"), proc.calls.Load())
	}
}

// A download still being sent at shutdown keeps its files until it is sent,
// and then they go at once: no timer runs after the program exits.
func TestDownloadCacheClearRemovesInUseEntryOnRelease(t *testing.T) {
	var c downloadCache
	prepare := func(_ context.Context, e *preparedDownload) {
		dir, err := os.MkdirTemp(t.TempDir(), "io-download-*")
		if err != nil {
			t.Error(err)
		}
		e.dir, e.path = dir, filepath.Join(dir, "a.jpg")
		_ = os.WriteFile(e.path, []byte("data"), 0o600)
	}
	sending := c.acquire("a", prepare)
	<-sending.done
	idle := c.acquire("b", prepare)
	<-idle.done
	c.release(idle) // kept for a minute, as usual

	c.clear()
	if _, err := os.Stat(idle.dir); !os.IsNotExist(err) {
		t.Error("an unused download was kept at shutdown")
	}
	if _, err := os.Stat(sending.path); err != nil {
		t.Fatalf("the download being sent lost its file: %v", err)
	}
	c.release(sending)
	if _, err := os.Stat(sending.dir); !os.IsNotExist(err) {
		t.Error("the download sent during shutdown left its files")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) != 0 {
		t.Errorf("%d entries left", len(c.entries))
	}
}
