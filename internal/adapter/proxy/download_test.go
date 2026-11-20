package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	var originals atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"message":"no"}`)
			return
		}
		name := map[string]string{jxl: "IMG 1.JXL", heic: "IMG_2.heic"}[strings.Split(r.URL.Path, "/")[3]]
		switch {
		case strings.HasSuffix(r.URL.Path, "/original"):
			originals.Add(1)
			w.Header().Set("Content-Type", "image/jxl")
			w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
			w.Header().Set("Cache-Control", "private, max-age=86400")
			_, _ = io.WriteString(w, "stored:"+r.URL.RawQuery)
		default:
			_, _ = io.WriteString(w, `{"originalFileName":"`+name+`"}`)
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
	// A part is a part of the converted file.
	if rec = get("/api/assets/"+jxl+"/original", map[string]string{"Range": "bytes=2-4"}); rec.Code != http.StatusPartialContent || rec.Body.String() != "234" {
		t.Errorf("range: %d %q", rec.Code, rec.Body.String())
	}

	// Not converted: another type, the edited version, and anything that is
	// not the download of an original.
	calls := proc.calls.Load()
	for _, path := range []string{"/api/assets/" + heic + "/original", "/api/assets/" + jxl + "/original?edited=true", "/api/assets/" + jxl + "/thumbnail"} {
		if rec = get(path, nil); proc.calls.Load() != calls {
			t.Errorf("%s was converted", path)
		}
	}
	if rec = get("/api/assets/"+jxl+"/original?edited=true", nil); rec.Body.String() != "stored:edited=true" {
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
