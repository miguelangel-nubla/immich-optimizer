package proxy

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

func TestAdmissionCountsWhatUploadsInProgressMayStillWrite(t *testing.T) {
	free := int64(1000)
	a := newAdmission(func() (int64, bool) { return free, true })
	a.wait = 50 * time.Millisecond
	ctx := context.Background()

	// Alone, an upload is always taken on, even one that cannot fit:
	// waiting would not make room for it.
	big := a.acquire(ctx, 5000)
	if big == nil {
		t.Fatal("the only upload was held back")
	}
	big.release()

	first := a.acquire(ctx, 100)
	// It has written its request (100) and may write two more copies.
	free = 900
	// 900 - 2*100 = 700 left for others: 3*200 fits, 3*250 does not.
	second := a.acquire(ctx, 200)
	if second == nil {
		t.Fatal("an upload that fits was held back")
	}
	second.release()
	if a.acquire(ctx, 250) != nil {
		t.Fatal("an upload was taken on without room for its copies")
	}

	// It is let in as soon as the one before it is done.
	done := make(chan *ticket)
	a.wait = 10 * time.Second
	go func() { done <- a.acquire(ctx, 250) }()
	time.Sleep(50 * time.Millisecond)
	first.release()
	first.release() // releasing twice is harmless
	select {
	case tk := <-done:
		if tk == nil {
			t.Fatal("not taken on after room was made")
		}
		tk.release()
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after room was made")
	}
	if a.count != 0 || a.bytes != 0 {
		t.Errorf("left over: %d upload(s), %d bytes", a.count, a.bytes)
	}

	// A size learned later is what counts.
	a.wait = 50 * time.Millisecond
	x := a.acquire(ctx, 0)
	x.resize(400)
	free = 1000
	if a.acquire(ctx, 100) != nil { // 1000 - 800 = 200 < 300
		t.Error("the corrected size was not counted")
	}
	x.release()

	// When free space cannot be told, nothing is held back.
	b := newAdmission(func() (int64, bool) { return 0, false })
	b.acquire(ctx, 10)
	if b.acquire(ctx, 1<<40) == nil {
		t.Error("held back although free space is unknown")
	}

	// A client that goes away stops waiting.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	y := a.acquire(ctx, 100)
	free = 0
	if a.acquire(gone, 100) != nil {
		t.Error("taken on for a client that is gone")
	}
	y.release()
}

func TestProxyHoldsUploadsWhenTheTempFolderIsFull(t *testing.T) {
	up := newUpstream(t)
	release := make(chan struct{})
	var started atomic.Int32
	inner := optimizedTo(t, "small")
	c := &fakeConverter{fn: func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
		started.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return inner.fn(ctx, in)
	}}
	server := newTestServer(t, up.srv.URL, c)
	free := int64(10 << 20)
	server.admission = newAdmission(func() (int64, bool) { return free, true })
	server.admission.wait = 100 * time.Millisecond

	// The first upload is being converted.
	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "a.jpg", "first"))
		firstDone <- rec.Code
	}()
	deadline := time.Now().Add(5 * time.Second)
	for started.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first upload never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// No room left: the next one is asked to come back, and is not converted.
	free = 0
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, uploadReq(t, "b.jpg", "second"))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q: want 503 with a Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if started.Load() != 1 {
		t.Fatal("an upload was converted although there was no room for it")
	}

	// Once the first one is through, the next is taken on again.
	close(release)
	if code := <-firstDone; code != http.StatusCreated {
		t.Fatalf("first upload: status %d", code)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		rec = httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "b.jpg", "second"))
		if rec.Code == http.StatusCreated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still refused after the first upload finished: %d", rec.Code)
		}
	}
}

func TestHealthPathIsAnsweredByTheProxyItself(t *testing.T) {
	up := newUpstream(t)
	server := newTestServer(t, up.srv.URL, optimizedTo(t, "small"))
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" || up.count() != 0 {
		t.Fatalf("health: %d %q, %d request(s) reached Immich", rec.Code, rec.Body.String(), up.count())
	}
	server.BeginShutdown()
	rec = httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthPath, nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health while shutting down: %d", rec.Code)
	}
}

// An upload's answer is kept for retries and for browsers that wait: what
// Immich meant for one connection or one client must not be in it.
func TestKeptAnswerCarriesNoCookieOrConnectionHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Set-Cookie", "immich_access_token=secret")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Immich-Cid", "abc")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"asset-1"}`)
	}))
	defer upstream.Close()
	server := newTestServer(t, upstream.URL, optimizedTo(t, "small"))
	for i := 0; i < 2; i++ { // the upload, then a retry served from memory
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, uploadReq(t, "a.jpg", "same"))
		h := rec.Header()
		if rec.Code != http.StatusCreated || h.Get("Set-Cookie") != "" || h.Get("Keep-Alive") != "" {
			t.Fatalf("answer %d: status %d, headers %v", i, rec.Code, h)
		}
		if h.Get("X-Immich-Cid") != "abc" || h.Get("Content-Type") != "application/json" {
			t.Fatalf("answer %d lost headers it should keep: %v", i, h)
		}
	}
}

// The mobile app's background upload names the asset in a form field and
// sends the file under the name of a temporary export. Immich uses the field.
func TestNameFieldIsTheNameToConvertAndToRename(t *testing.T) {
	const temp = "59DC5D49-1302-4D2E-BEE2-4F727646A630_L0_001_1773008161.884566_o_IMG_0992.HEIC"
	for _, fieldFirst := range []bool{true, false} {
		up := newUpstream(t)
		var converted string
		inner := optimizedTo(t, "small")
		c := &fakeConverter{fn: func(ctx context.Context, in entity.ConvertInput) (*entity.ConvertResult, error) {
			converted = in.Filename
			return inner.fn(ctx, in)
		}}
		server := newTestServer(t, up.srv.URL, c)

		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		if fieldFirst {
			_ = mw.WriteField("filename", "IMG_0992.HEIC")
		}
		fw, _ := mw.CreateFormFile("assetData", temp)
		_, _ = fw.Write([]byte("photo"))
		if !fieldFirst {
			_ = mw.WriteField("filename", "IMG_0992.HEIC")
		}
		_ = mw.WriteField("deviceId", "phone-1")
		_ = mw.Close()
		req := httptest.NewRequest(http.MethodPost, "/api/assets", body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
		if converted != "IMG_0992.HEIC" {
			t.Errorf("converted under the name %q, want the one in the field", converted)
		}
		sent := up.bodies[0]
		if !strings.Contains(sent, "name=\"filename\"\r\n\r\nIMG_0992.jxl\r\n") {
			t.Errorf("the name field was not given the converted name (field first: %v):\n%s", fieldFirst, sent)
		}
		if !strings.Contains(sent, `filename="IMG_0992.jxl"`) || strings.Contains(sent, "L0_001") || strings.Contains(sent, "HEIC") {
			t.Errorf("the file part does not carry the converted name alone (field first: %v):\n%s", fieldFirst, sent)
		}
	}
}

// An upload whose request is still coming in may write all three copies yet;
// one whose size the client did not say cannot be counted at all.
func TestAdmissionCountsRequestsStillBeingReceived(t *testing.T) {
	free := int64(1000)
	a := newAdmission(func() (int64, bool) { return free, true })
	a.wait = 50 * time.Millisecond
	ctx := context.Background()

	first := a.acquire(ctx, 200) // nothing of it on disk yet
	// 1000 - 3*200 = 400 left: 3*100 fits, 3*150 does not.
	if a.acquire(ctx, 150) != nil {
		t.Fatal("taken on as if the first request were already on disk")
	}
	small := a.acquire(ctx, 100)
	if small == nil {
		t.Fatal("an upload that fits was held back")
	}
	small.release()
	first.release()

	// Unknown size: not while another request comes in...
	receiving := a.acquire(ctx, 10)
	if a.acquire(ctx, -1) != nil {
		t.Fatal("an upload of unknown size was taken on while another request came in")
	}
	// ...but next to uploads already received (being converted).
	receiving.resize(10)
	converting := receiving
	chunked := a.acquire(ctx, -1)
	if chunked == nil {
		t.Fatal("an upload of unknown size was held back by one being converted")
	}
	if a.acquire(ctx, 1) != nil {
		t.Fatal("taken on while an upload of unknown size was being received")
	}
	if a.acquire(ctx, -1) != nil {
		t.Fatal("a second upload of unknown size was taken on")
	}
	// Once it is on disk its size is known, and the others are let in.
	done := make(chan *ticket)
	a.wait = 5 * time.Second
	go func() { done <- a.acquire(ctx, 10) }()
	time.Sleep(50 * time.Millisecond)
	chunked.resize(100)
	tk := <-done
	if tk == nil {
		t.Fatal("not taken on once the size was known")
	}
	tk.release()
	chunked.release()
	converting.release()
	if a.count != 0 || a.bytes != 0 || a.unspooled != 0 || a.unknown != 0 {
		t.Errorf("left over: %d upload(s), %d bytes, %d unspooled, %d unknown", a.count, a.bytes, a.unspooled, a.unknown)
	}
}
