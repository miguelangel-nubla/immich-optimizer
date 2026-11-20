package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
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
