package proxy

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/httpclient"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/undone"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/usecase/convert"
)

// ---- fakes ----

type fakeConverter struct {
	fn    func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error)
	calls atomic.Int32
}

func (f *fakeConverter) Convert(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
	f.calls.Add(1)
	return f.fn(ctx, in)
}

func (f *fakeConverter) SaveUndone(entity.ConvertInput, entity.Failure) (*entity.UndoneRecord, error) {
	return nil, errors.New("not used by proxy")
}

// optimizedTo returns a converter that delivers content under a .jxl name.
func optimizedTo(t *testing.T, content string) *fakeConverter {
	return &fakeConverter{fn: func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		p := filepath.Join(t.TempDir(), "out.jxl")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return nil, err
		}
		return &entity.ConvertResult{
			Outcome: entity.OutcomeOptimized, DeliverPath: p,
			DeliverName: strings.TrimSuffix(in.Filename, filepath.Ext(in.Filename)) + ".jxl",
			Release:     func() { _ = os.Remove(p) },
		}, nil
	}}
}

type upstreamRecorder struct {
	mu     sync.Mutex
	bodies []string
	keys   []string
	got    chan struct{}
	srv    *httptest.Server
	// gone makes every asset look deleted; checks counts the questions about
	// whether an asset still exists.
	gone   bool
	checks int
}

func newUpstream(t *testing.T) *upstreamRecorder {
	u := &upstreamRecorder{got: make(chan struct{}, 100)}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/assets/") {
			u.mu.Lock()
			u.checks++
			gone := u.gone
			u.mu.Unlock()
			if gone {
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.bodies = append(u.bodies, string(b))
		u.keys = append(u.keys, r.Header.Get("x-api-key"))
		n := len(u.bodies)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"asset-%d"}`, n)
		u.got <- struct{}{}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *upstreamRecorder) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bodies)
}

func quiet() *customlogger.Logger { return customlogger.NewWithLevel(nil, "error", "") }

func newTestServer(t *testing.T, upstreamURL string, c port.Converter, mod ...func(*Config)) *Server {
	t.Helper()
	u, _ := url.Parse(upstreamURL)
	cfg := Config{UpstreamURL: u, BindAddr: "127.0.0.1:0"}
	for _, m := range mod {
		m(&cfg)
	}
	s := NewServer(cfg, c, quiet())
	t.Cleanup(func() { s.Cancel() })
	return s
}

func uploadBody(t *testing.T, filename, content string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("deviceAssetId", "a-1")
	fw, _ := mw.CreateFormFile("assetData", filename)
	_, _ = fw.Write([]byte(content))
	_ = mw.WriteField("deviceId", "phone-1")
	_ = mw.WriteField("fileModifiedAt", "2024-03-04T05:06:07.000Z")
	_ = mw.Close()
	return body, mw.FormDataContentType()
}

func uploadReq(t *testing.T, filename, content string) *http.Request {
	body, ct := uploadBody(t, filename, content)
	req := httptest.NewRequest(http.MethodPost, "/api/assets", body)
	req.Header.Set("Content-Type", ct)
	return req
}

// ---- tests ----

func TestProxyPassthroughNonUpload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/server-info" {
			_, _ = w.Write([]byte(`{"res":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	server := newTestServer(t, upstream.URL, &fakeConverter{})
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/server-info", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != `{"res":"ok"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyUploadOptimization(t *testing.T) {
	up := newUpstream(t)
	c := optimizedTo(t, "small")
	var seen entity.ConvertInput
	inner := c.fn
	c.fn = func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		seen = in
		return inner(ctx, in)
	}
	server := newTestServer(t, up.srv.URL, c)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, uploadReq(t, "original.jpg", strings.Repeat("A", 100)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	body := up.bodies[0]
	if !strings.Contains(body, "small") || strings.Contains(body, strings.Repeat("A", 100)) {
		t.Fatalf("upstream did not get the optimized content: %s", body)
	}
	if !strings.Contains(body, `filename="original.jxl"`) {
		t.Fatalf("expected original basename with new extension: %s", body)
	}
	// Other fields are forwarded in their original order.
	if !(strings.Index(body, "deviceAssetId") < strings.Index(body, "assetData") && strings.Index(body, "assetData") < strings.Index(body, "deviceId")) {
		t.Fatalf("part order changed: %s", body)
	}
	if seen.Source != entity.SourceProxy || seen.Filename != "original.jpg" || seen.SHA1 != sha1Hex(strings.Repeat("A", 100)) ||
		seen.Details["device_id"] != "phone-1" || seen.ModTime.IsZero() {
		t.Fatalf("unexpected ConvertInput %+v", seen)
	}
}

func TestProxyUploadNonMatchingExtensionBlocked(t *testing.T) {
	up := newUpstream(t)
	c := &fakeConverter{fn: func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		return nil, &entity.ConversionError{Filename: in.Filename, Failure: entity.Failure{Stage: entity.StageNoTask, Err: entity.ErrNoTask}}
	}}
	server := newTestServer(t, up.srv.URL, c)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, uploadReq(t, "document.pdf", "pdf"))
	if rec.Code != http.StatusUnsupportedMediaType || !strings.Contains(rec.Body.String(), ".pdf") {
		t.Fatalf("expected 415, got %d %s", rec.Code, rec.Body.String())
	}
	if up.count() != 0 {
		t.Fatalf("upstream must not be called")
	}
}

func asyncReq(t *testing.T, filename, content string) *http.Request {
	req := uploadReq(t, filename, content)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return req
}

func startAsync(t *testing.T, s *Server, req *http.Request) (string, *job) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, WaitPath+"?job=") {
		t.Fatalf("unexpected redirect %s", loc)
	}
	j := s.jobs.get(strings.TrimPrefix(loc, WaitPath+"?job="))
	if j == nil {
		t.Fatalf("job not registered")
	}
	return loc, j
}

func wait(s *Server, loc string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, loc, strings.NewReader("re-posted body")))
	return rec
}

func TestProxyAsyncJobRedirect(t *testing.T) {
	up := newUpstream(t)
	server := newTestServer(t, up.srv.URL, optimizedTo(t, "x"))
	loc, _ := startAsync(t, server, asyncReq(t, "image.jpg", "image-data"))
	rec := wait(server, loc)
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":"asset-1"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyAsyncFailureVisibleAfterJobEnds(t *testing.T) {
	up := newUpstream(t)
	c := &fakeConverter{fn: func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		return nil, &entity.ConversionError{Filename: in.Filename, Failure: entity.Failure{Stage: entity.StageConvert, Err: errors.New("bad")}}
	}}
	server := newTestServer(t, up.srv.URL, c)
	loc, j := startAsync(t, server, asyncReq(t, "image.jpg", "x"))
	<-j.done
	for i := 0; i < 2; i++ {
		if rec := wait(server, loc); rec.Code != http.StatusBadGateway {
			t.Fatalf("wait #%d: expected 502, got %d", i+1, rec.Code)
		}
	}
}

func TestProxyAsyncSuccessAfterJobEnds(t *testing.T) {
	up := newUpstream(t)
	server := newTestServer(t, up.srv.URL, optimizedTo(t, "x"))
	loc, j := startAsync(t, server, asyncReq(t, "image.jpg", "x"))
	<-j.done
	rec := wait(server, loc)
	if rec.Code != http.StatusCreated || rec.Body.String() != `{"id":"asset-1"}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyAsyncWaitRedirectsWhileRunning(t *testing.T) {
	up := newUpstream(t)
	release := make(chan struct{})
	c := optimizedTo(t, "x")
	inner := c.fn
	c.fn = func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		<-release
		return inner(ctx, in)
	}
	server := newTestServer(t, up.srv.URL, c, func(cfg *Config) { cfg.WaitPoll = 10 * time.Millisecond })
	loc, j := startAsync(t, server, asyncReq(t, "image.jpg", "x"))
	if rec := wait(server, loc); rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected keep-alive redirect, got %d", rec.Code)
	}
	close(release)
	<-j.done
	if rec := wait(server, loc); rec.Code != http.StatusCreated {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestProxyWaitUnknownJob404(t *testing.T) {
	server := newTestServer(t, "http://127.0.0.1:1", &fakeConverter{})
	if rec := wait(server, WaitPath+"?job=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// startReal serves the proxy on a real listener (needed where disconnects matter).
func startReal(t *testing.T, s *Server) string {
	t.Helper()
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return "http://" + s.Addr()
}

func TestProxySyncSurvivesClientDisconnect(t *testing.T) {
	up := newUpstream(t)
	started := make(chan struct{})
	release := make(chan struct{})
	ctxErr := make(chan error, 1)
	c := optimizedTo(t, "small")
	inner := c.fn
	c.fn = func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		close(started)
		<-release
		ctxErr <- ctx.Err()
		return inner(ctx, in)
	}
	server := newTestServer(t, up.srv.URL, c)
	base := startReal(t, server)

	body, ct := uploadBody(t, "a.jpg", "AAAA")
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/assets", body)
	req.Header.Set("Content-Type", ct)
	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()

	<-started
	cancel()
	if err := <-clientDone; err == nil {
		t.Fatalf("client request should have been aborted")
	}
	close(release)

	select {
	case <-up.got:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream never received the upload after client disconnect")
	}
	if err := <-ctxErr; err != nil {
		t.Fatalf("converter ctx was cancelled by the client disconnect: %v", err)
	}
	if !strings.Contains(up.bodies[0], "small") {
		t.Fatalf("upstream got wrong body")
	}
}

func TestProxyNotBoundByHTTPTimeout(t *testing.T) {
	up := newUpstream(t)
	c := optimizedTo(t, "small")
	inner := c.fn
	c.fn = func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		select {
		case <-time.After(800 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return inner(ctx, in)
	}
	hc := httpclient.New(httpclient.Options{ResponseHeaderTimeout: 300 * time.Millisecond})
	server := newTestServer(t, up.srv.URL, c, func(cfg *Config) { cfg.HTTPClient = hc })
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, uploadReq(t, "a.jpg", "AAAA"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 despite conversion longer than http timeout, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestProxyCoalescesIdenticalUploads(t *testing.T) {
	up := newUpstream(t)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	c := optimizedTo(t, "small")
	inner := c.fn
	c.fn = func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		started <- struct{}{}
		<-release
		return inner(ctx, in)
	}
	server := newTestServer(t, up.srv.URL, c)

	recs := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range recs {
		recs[i] = httptest.NewRecorder()
		req := uploadReq(t, "same.jpg", "identical-content")
		req.Header.Set("x-api-key", "user-a")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			server.ServeHTTP(recs[i], req)
		}(i)
		if i == 0 {
			<-started
		}
	}
	close(release)
	wg.Wait()

	if n := c.calls.Load(); n != 1 {
		t.Fatalf("converter called %d times, want 1", n)
	}
	if up.count() != 1 {
		t.Fatalf("upstream called %d times, want 1", up.count())
	}
	for i, r := range recs {
		if r.Code != http.StatusCreated || r.Body.String() != `{"id":"asset-1"}` {
			t.Fatalf("response %d: %d %s", i, r.Code, r.Body.String())
		}
	}
}

func TestProxyNoCoalesceAcrossAuth(t *testing.T) {
	up := newUpstream(t)
	server := newTestServer(t, up.srv.URL, optimizedTo(t, "small"))
	for _, key := range []string{"user-a", "user-b"} {
		req := uploadReq(t, "same.jpg", "identical-content")
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d", rec.Code)
		}
	}
	if up.count() != 2 {
		t.Fatalf("different users must not be coalesced; uploads=%d", up.count())
	}
}

// Two uploads with identical bytes but different form fields (here the
// live-photo pairing) are different assets and must never share a result.
func TestProxyNoCoalesceAcrossFormFields(t *testing.T) {
	up := newUpstream(t)
	server := newTestServer(t, up.srv.URL, optimizedTo(t, "small"))
	var ids []string
	for _, live := range []string{"video-1", "video-2"} {
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		_ = mw.WriteField("deviceAssetId", "a-1")
		_ = mw.WriteField("livePhotoVideoId", live)
		fw, _ := mw.CreateFormFile("assetData", "same.jpg")
		_, _ = fw.Write([]byte("identical-content"))
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/assets", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("x-api-key", "user-a")
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d", rec.Code)
		}
		ids = append(ids, rec.Body.String())
	}
	if up.count() != 2 || ids[0] == ids[1] {
		t.Fatalf("uploads differing in livePhotoVideoId were coalesced: uploads=%d ids=%v", up.count(), ids)
	}
}

func TestCoalesceKeyComponents(t *testing.T) {
	base := func() jobSpec {
		u, _ := url.Parse("/api/assets?key=x")
		h := http.Header{}
		h.Set("Cookie", "immich_access_token=a")
		return jobSpec{method: http.MethodPost, url: u, header: h}
	}
	ref := coalesceKey("sha", "form", base())
	if ref != coalesceKey("sha", "form", base()) {
		t.Fatalf("key must be deterministic")
	}
	variants := map[string]func(*jobSpec) (string, string){
		"file":  func(*jobSpec) (string, string) { return "sha2", "form" },
		"form":  func(*jobSpec) (string, string) { return "sha", "form2" },
		"path":  func(s *jobSpec) (string, string) { s.url.Path = "/api/other"; return "sha", "form" },
		"query": func(s *jobSpec) (string, string) { s.url.RawQuery = "key=y"; return "sha", "form" },
		"cookie": func(s *jobSpec) (string, string) {
			s.header.Set("Cookie", "immich_access_token=b")
			return "sha", "form"
		},
		"bearer": func(s *jobSpec) (string, string) { s.header.Set("Authorization", "Bearer t"); return "sha", "form" },
		"apikey": func(s *jobSpec) (string, string) { s.header.Set("X-Api-Key", "k"); return "sha", "form" },
	}
	for name, mod := range variants {
		s := base()
		file, form := mod(&s)
		if coalesceKey(file, form, s) == ref {
			t.Errorf("%s must change the coalescing key", name)
		}
	}
	if strings.Contains(ref, "immich_access_token") {
		t.Fatalf("key must not contain credentials in clear: %s", ref)
	}
}

func TestSplitClientFilename(t *testing.T) {
	cases := []struct{ raw, dir, base string }{
		{"IMG_1.HEIC", "", "IMG_1.HEIC"},
		{"DCIM/Camera/IMG_1.HEIC", "DCIM/Camera", "IMG_1.HEIC"},
		// A backslash is a legal filename character, not a separator.
		{`C:\Photos\a.jpg`, "", `C:\Photos\a.jpg`},
		{`holiday\beach.jpg`, "", `holiday\beach.jpg`},
	}
	for _, c := range cases {
		if d, b := splitClientFilename(c.raw); d != c.dir || b != c.base {
			t.Errorf("splitClientFilename(%q) = %q, %q", c.raw, d, b)
		}
	}
}

// The Immich mobile app aborts a whole backup run when an upload response
// contains this exact text, so proxy-generated errors must never carry it,
// whatever the underlying error says.
func TestProxyErrorsNeverContainQuotaText(t *testing.T) {
	const quota = "Quota has been exceeded!"
	cause := errors.New(`upload failed with status 400: {"message":"` + quota + `"}`)
	errs := []error{
		cause,
		fmt.Errorf("%w: %w", errUpstream, cause),
		fmt.Errorf("%w: %w", errBadRequest, cause),
		fmt.Errorf("%w: %w", entity.ErrCanceled, cause),
		&entity.ConversionError{Filename: "a.jpg", Failure: entity.Failure{Stage: entity.StageConvert, Err: cause}},
		&entity.ConversionError{Filename: "a.pdf", Failure: entity.Failure{Stage: entity.StageNoTask, Err: cause}},
		&entity.ConversionError{Filename: "a.jpg", Failure: entity.Failure{Stage: entity.StageUpload, Err: cause}},
	}
	for _, err := range errs {
		res := resultFromError(err)
		if strings.Contains(strings.ToLower(string(res.body)), "quota") {
			t.Errorf("proxy error body for %v contains quota text: %q", err, res.body)
		}
	}
}

func TestProxyRetryAfterSuccessGetsCachedResponse(t *testing.T) {
	up := newUpstream(t)
	c := optimizedTo(t, "small")
	server := newTestServer(t, up.srv.URL, c)
	var bodies []string
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "a.jpg", "same"))
		bodies = append(bodies, rec.Body.String())
	}
	if up.count() != 1 || c.calls.Load() != 1 || bodies[0] != bodies[1] {
		t.Fatalf("retry should reuse the cached result: uploads=%d calls=%d bodies=%v", up.count(), c.calls.Load(), bodies)
	}
}

// A remembered answer names an asset. When that asset has been deleted from
// Immich in the meantime, the same upload must be stored again instead of
// being answered with the id of something that is gone.
func TestProxyRetryAfterTheAssetWasDeletedUploadsAgain(t *testing.T) {
	up := newUpstream(t)
	c := optimizedTo(t, "small")
	server := newTestServer(t, up.srv.URL, c)
	send := func() string {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "a.jpg", "same"))
		return rec.Body.String()
	}
	first := send()
	up.mu.Lock()
	up.gone = true
	up.mu.Unlock()
	second := send()
	if up.count() != 2 || c.calls.Load() != 2 || second == first {
		t.Fatalf("the upload was not repeated: uploads=%d conversions=%d answers=%q %q", up.count(), c.calls.Load(), first, second)
	}
	// The new answer is remembered in place of the old one.
	up.mu.Lock()
	up.gone = false
	up.mu.Unlock()
	if third := send(); third != second || up.count() != 2 {
		t.Fatalf("the repeated upload was not remembered: uploads=%d answers=%q %q", up.count(), second, third)
	}
}

func TestProxyIncompleteBodyNotProcessed(t *testing.T) {
	c := &fakeConverter{fn: func(context.Context, entity.ConvertInput) (*entity.ConvertResult, error) {
		t.Errorf("converter must not be called for an incomplete body")
		return nil, errors.New("x")
	}}
	server := newTestServer(t, "http://127.0.0.1:1", c)
	base := startReal(t, server)

	body, ct := uploadBody(t, "a.jpg", strings.Repeat("A", 4096))
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /api/assets HTTP/1.1\r\nHost: x\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", ct, body.Len())
	_, _ = conn.Write(body.Bytes()[:body.Len()/2])
	conn.Close()

	// Give the handler time to observe the short body; no job may start.
	time.Sleep(200 * time.Millisecond)
	if c.calls.Load() != 0 {
		t.Fatalf("converter called")
	}
}

func TestProxyDeliverOpenFailureDoesNotForwardOriginal(t *testing.T) {
	up := newUpstream(t)
	c := &fakeConverter{fn: func(context.Context, entity.ConvertInput) (*entity.ConvertResult, error) {
		return &entity.ConvertResult{Outcome: entity.OutcomeOptimized, DeliverPath: "/nonexistent/out.jxl", DeliverName: "a.jxl", Release: func() {}}, nil
	}}
	server := newTestServer(t, up.srv.URL, c)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, uploadReq(t, "a.jpg", "original"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("the original must never be forwarded")
	}
}

func TestProxyShutdownCancelsJobs(t *testing.T) {
	up := newUpstream(t)
	started := make(chan struct{})
	ctxErr := make(chan error, 1)
	c := &fakeConverter{fn: func(ctx context.Context, _ entity.ConvertInput) (*entity.ConvertResult, error) {
		close(started)
		<-ctx.Done()
		ctxErr <- ctx.Err()
		return nil, fmt.Errorf("%w: %w", entity.ErrCanceled, context.Cause(ctx))
	}}
	server := newTestServer(t, up.srv.URL, c)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(rec, uploadReq(t, "a.jpg", "x"))
		close(done)
	}()
	<-started

	expired, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := server.Drain(expired); err == nil {
		t.Fatalf("drain should report the running job")
	}
	server.Cancel()
	<-done
	if err := <-ctxErr; err == nil {
		t.Fatalf("converter ctx not cancelled")
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 503 + Retry-After, got %d", rec.Code)
	}
	// New uploads are refused while shutting down.
	rec2 := httptest.NewRecorder()
	server.ServeHTTP(rec2, uploadReq(t, "b.jpg", "y"))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for new upload during shutdown, got %d", rec2.Code)
	}
}

// ---- integration with the real engine and undone store ----

type mockProcessor struct {
	fn func(ctx context.Context, filePath string) (*entity.ProcessResult, error)
}

func (m *mockProcessor) Process(ctx context.Context, filePath string, _ []entity.Task) (*entity.ProcessResult, error) {
	return m.fn(ctx, filePath)
}

func realEngine(t *testing.T, proc *mockProcessor, saveUnmatched bool) (*convert.Engine, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "undone")
	store, err := undone.NewLocalStore(root, "test", quiet())
	if err != nil {
		t.Fatal(err)
	}
	e := convert.New(convert.Config{
		Tasks:         []entity.Task{{Name: "jpg", Extensions: []string{"jpg"}}},
		MaxConcurrent: 2,
		SaveUnmatched: saveUnmatched,
	}, proc, store, quiet())
	return e, store.Root()
}

func TestProxyFailureSavesUndone(t *testing.T) {
	up := newUpstream(t)
	proc := &mockProcessor{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		return nil, &entity.ProcessError{Attempts: []entity.TaskAttempt{{Task: "jpg", ExitCode: 1}}, Err: errors.New("corrupt")}
	}}
	e, root := realEngine(t, proc, true)
	server := newTestServer(t, up.srv.URL, e, func(c *Config) { c.ResultTTL = time.Nanosecond })

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "original.jpg", "jpeg-bytes"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("expected 502, got %d", rec.Code)
		}
		time.Sleep(2 * time.Millisecond) // let the 1ns TTL expire so the retry re-runs
	}
	if up.count() != 0 {
		t.Fatalf("upstream must never be called on failure")
	}
	dir := filepath.Join(root, sha1Hex("jpeg-bytes"))
	if b, err := os.ReadFile(filepath.Join(dir, undone.OriginalFile)); err != nil || string(b) != "jpeg-bytes" {
		t.Fatalf("original not saved to undone: %v %q", err, b)
	}
	note, err := os.ReadFile(filepath.Join(dir, undone.MetadataFile))
	if err != nil || !strings.Contains(string(note), `"occurrences": 2`) || !strings.Contains(string(note), `"source": "proxy"`) ||
		!strings.Contains(string(note), `"original_filename": "original.jpg"`) {
		t.Fatalf("metadata wrong: %v %s", err, note)
	}
	entries, _ := os.ReadDir(root)
	var folders []string
	for _, e := range entries {
		if e.Name() != undone.LockFile {
			folders = append(folders, e.Name())
		}
	}
	if len(folders) != 1 {
		t.Fatalf("retries must not pile up copies: %v", folders)
	}
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// The undone metadata must carry the client's multipart filename (and the
// directory part, if the client sent one), never the proxy's temp name.
func TestProxyUndoneRecordsClientFilename(t *testing.T) {
	up := newUpstream(t)
	proc := &mockProcessor{fn: func(context.Context, string) (*entity.ProcessResult, error) {
		return nil, errors.New("corrupt")
	}}
	e, root := realEngine(t, proc, true)
	server := newTestServer(t, up.srv.URL, e)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, uploadReq(t, "DCIM/Camera/IMG_0042.jpg", "client-bytes"))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
	b, err := os.ReadFile(filepath.Join(root, sha1Hex("client-bytes"), undone.MetadataFile))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		OriginalFilenames []string `json:"original_filenames"`
		Failures          []struct {
			Source           string `json:"source"`
			OriginalFilename string `json:"original_filename"`
			OriginalDir      string `json:"original_dir"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Failures) != 1 || m.Failures[0].OriginalFilename != "IMG_0042.jpg" || m.Failures[0].OriginalDir != "DCIM/Camera" ||
		m.Failures[0].Source != "proxy" || len(m.OriginalFilenames) != 1 || m.OriginalFilenames[0] != "IMG_0042.jpg" {
		t.Fatalf("metadata does not carry the client filename: %s", b)
	}
	if strings.Contains(string(b), "io-proxy-") {
		t.Fatalf("metadata leaks a proxy temp name: %s", b)
	}
}

func TestProxyNoTaskSavesUndone(t *testing.T) {
	for _, save := range []bool{true, false} {
		up := newUpstream(t)
		proc := &mockProcessor{fn: func(context.Context, string) (*entity.ProcessResult, error) {
			t.Fatalf("processor must not run for unmatched files")
			return nil, nil
		}}
		e, root := realEngine(t, proc, save)
		server := newTestServer(t, up.srv.URL, e)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "doc.pdf", "pdf"))
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("expected 415, got %d", rec.Code)
		}
		_, err := os.Stat(filepath.Join(root, sha1Hex("pdf"), undone.OriginalFile))
		if save && err != nil {
			t.Fatalf("unmatched file should be saved: %v", err)
		}
		if !save && err == nil {
			t.Fatalf("unmatched file must not be saved with SaveUnmatched=false")
		}
	}
}

func TestProxyUnchangedDeliversOriginalName(t *testing.T) {
	up := newUpstream(t)
	proc := &mockProcessor{fn: func(_ context.Context, p string) (*entity.ProcessResult, error) {
		out := filepath.Join(t.TempDir(), "x.jxl")
		_ = os.WriteFile(out, []byte("bigger-than-input"), 0o600)
		st, _ := os.Stat(p)
		return &entity.ProcessResult{ProcessedFilePath: out, OriginalSize: st.Size(), ProcessedSize: 17}, nil
	}}
	e, _ := realEngine(t, proc, true)
	server := newTestServer(t, up.srv.URL, e)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, uploadReq(t, "IMG.jpg", "tiny"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d", rec.Code)
	}
	if !strings.Contains(up.bodies[0], `filename="IMG.jpg"`) || !strings.Contains(up.bodies[0], "tiny") {
		t.Fatalf("expected original delivered: %s", up.bodies[0])
	}
}

// ---- review fixes ----

// A job that finishes during the grace period must still reach its client:
// FinishShutdown waits for the handler to write the result before closing.
func TestProxyShutdownDeliversFinishedResult(t *testing.T) {
	up := newUpstream(t)
	started := make(chan struct{})
	release := make(chan struct{})
	c := optimizedTo(t, "small")
	inner := c.fn
	c.fn = func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		close(started)
		<-release
		return inner(ctx, in)
	}
	server := newTestServer(t, up.srv.URL, c)
	base := startReal(t, server)

	body, ct := uploadBody(t, "a.jpg", "AAAA")
	req, _ := http.NewRequest(http.MethodPost, base+"/api/assets", body)
	req.Header.Set("Content-Type", ct)
	type result struct {
		code int
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		got <- result{code: resp.StatusCode}
	}()
	<-started
	server.BeginShutdown()
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Drain(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	fctx, fcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer fcancel()
	_ = server.FinishShutdown(fctx)
	r := <-got
	if r.err != nil || r.code != http.StatusCreated {
		t.Fatalf("client must get the finished result, got %d %v", r.code, r.err)
	}
}

// eofSignalReader closes done once the whole body has been read.
type eofSignalReader struct {
	r    io.Reader
	done chan struct{}
	once sync.Once
}

func (e *eofSignalReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		e.once.Do(func() { close(e.done) })
	}
	return n, err
}

func countTemp(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// A coalesced duplicate must not hold its own spool and source copy while it
// waits for the original job.
func TestProxyCoalescedDuplicateFreesDisk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	up := newUpstream(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	c := optimizedTo(t, "small")
	inner := c.fn
	c.fn = func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		started <- struct{}{}
		<-release
		return inner(ctx, in)
	}
	server := newTestServer(t, up.srv.URL, c)

	var wg sync.WaitGroup
	serve := func(req *http.Request) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	serve(uploadReq(t, "big.mov", "identical"))
	<-started
	if n := countTemp(t, tmp); n != 2 {
		t.Fatalf("first job should hold spool + source, got %d files", n)
	}

	body, ct := uploadBody(t, "big.mov", "identical")
	sig := &eofSignalReader{r: body, done: make(chan struct{})}
	req := httptest.NewRequest(http.MethodPost, "/api/assets", sig)
	req.Header.Set("Content-Type", ct)
	serve(req)
	<-sig.done

	deadline := time.Now().Add(3 * time.Second)
	for countTemp(t, tmp) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("duplicate still holds its copies while waiting: %d files", countTemp(t, tmp))
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	wg.Wait()
	if c.calls.Load() != 1 || up.count() != 1 {
		t.Fatalf("calls=%d uploads=%d", c.calls.Load(), up.count())
	}
}

func TestCacheableStatus(t *testing.T) {
	for code, want := range map[int]bool{
		200: true, 201: true, 400: true, 401: true, 403: true, 409: true,
		408: false, 413: false, 425: false, 429: false, 500: false, 502: false, 503: false, 101: false,
	} {
		if got := cacheableStatus(code); got != want {
			t.Errorf("cacheableStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

// A transient upstream answer (429) is not replayed to an identical retry.
func TestProxyTransientUpstreamNotCached(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, optimizedTo(t, "small"))
	codes := []int{}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "a.jpg", "same"))
		codes = append(codes, rec.Code)
	}
	if hits.Load() != 2 || codes[0] != http.StatusTooManyRequests || codes[1] != http.StatusCreated {
		t.Fatalf("retry after 429 must reach Immich: hits=%d codes=%v", hits.Load(), codes)
	}
}

// When saving the original to undone failed, an identical retry must be
// processed (and saved) again instead of getting a cached 502.
func TestProxyUndoneSaveFailureNotCached(t *testing.T) {
	up := newUpstream(t)
	c := &fakeConverter{fn: func(_ context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		return nil, &entity.ConversionError{
			Filename:  in.Filename,
			Failure:   entity.Failure{Stage: entity.StageConvert, Err: errors.New("corrupt")},
			UndoneErr: errors.New("undone dir full"),
		}
	}}
	server := newTestServer(t, up.srv.URL, c)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "a.jpg", "same"))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("expected 502, got %d", rec.Code)
		}
	}
	if c.calls.Load() != 2 {
		t.Fatalf("retry must be processed again, converter calls=%d", c.calls.Load())
	}
	saved := resultFromError(&entity.ConversionError{Failure: entity.Failure{Stage: entity.StageConvert, Err: errors.New("x")}, Undone: &entity.UndoneRecord{}})
	if !saved.cacheable {
		t.Fatalf("failure saved to undone should stay cacheable")
	}
}
