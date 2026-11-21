// Package proxy is the reverse-proxy source: it intercepts Immich upload
// requests, hands the file to the conversion engine and uploads the result.
//
// Jobs are detached from the client connection. The request body is first
// spooled to disk (bound to the client), then conversion and the upstream
// upload run on a server-lifetime context that is cancelled only on
// shutdown. A client that disconnects or times out does not stop the job,
// and an identical retry is coalesced onto the running or recently finished
// job instead of being processed and uploaded twice.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/httpclient"
	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

// WaitPath is the browser keep-alive redirect target.
const WaitPath = "/_immich-optimizer/wait"

// HealthPath is answered by the proxy itself: 200 while it takes uploads.
const HealthPath = "/_immich-optimizer/health"

const (
	defaultResultTTL   = 30 * time.Minute
	defaultWaitPoll    = 55 * time.Second
	defaultMaxKeptJobs = 10_000
)

type Config struct {
	UpstreamURL *url.URL
	BindAddr    string
	// PassUploads sends uploads to Immich as they are, like any other
	// request: library mode converts them later, when they are tagged.
	PassUploads bool
	// PassDownloads sends downloads as Immich makes them, whatever the
	// Downloads tasks say.
	PassDownloads bool
	// Downloads are the tasks that convert an original on its way out, run
	// by Processor. Without them downloads pass through untouched.
	Downloads []entity.Task
	Processor port.MediaProcessor
	// DownloadTimeout bounds one such conversion; 0 means no limit.
	DownloadTimeout time.Duration
	// DownloadConcurrency is how many run at once. Default 2.
	DownloadConcurrency int
	// Audit, when set, records every upload that reached Immich.
	Audit *audit.Log
	// HTTPClient is used for the upstream upload. It should have
	// transport-level timeouts only (see httpclient.New).
	HTTPClient *http.Client
	// ResultTTL is how long finished results are kept for coalescing retries
	// and late /wait requests. Default 30m.
	ResultTTL time.Duration
	// WaitPoll is how long a /wait request blocks before redirecting again.
	// Default 55s.
	WaitPoll time.Duration
	// MaxKeptJobs bounds retained finished jobs. Default 10000.
	MaxKeptJobs int
}

type Server struct {
	cfg      Config
	conv     port.Converter
	logger   *logger.Logger
	revProxy *httputil.ReverseProxy
	server   *http.Server
	client   *http.Client
	jobs     *jobManager
	// admission holds uploads back while the temp folder has no room.
	admission *admission
	// streamClient has no timeouts: a sync stream can take as long as it takes.
	streamClient *http.Client
	users        userCache
	names        nameCache
	downloadSem  chan struct{}

	baseCtx    context.Context
	cancelBase context.CancelCauseFunc

	// lifecycle guards jobsWG.Add against a concurrent Drain.
	lifecycle    sync.Mutex
	shuttingDown bool
	jobsWG       sync.WaitGroup

	listenerMu sync.Mutex
	listener   net.Listener
}

func NewServer(cfg Config, conv port.Converter, log *logger.Logger) *Server {
	if cfg.ResultTTL <= 0 {
		cfg.ResultTTL = defaultResultTTL
	}
	if cfg.WaitPoll <= 0 {
		cfg.WaitPoll = defaultWaitPoll
	}
	if cfg.MaxKeptJobs <= 0 {
		cfg.MaxKeptJobs = defaultMaxKeptJobs
	}
	client := cfg.HTTPClient
	if client == nil {
		client = httpclient.New(httpclient.Options{})
	}

	baseCtx, cancel := context.WithCancelCause(context.Background())

	// Pass-through traffic keeps the default transport: some Immich endpoints
	// (downloads, websockets) can legitimately take long to send headers.
	revProxy := httputil.NewSingleHostReverseProxy(cfg.UpstreamURL)

	s := &Server{
		cfg:          cfg,
		conv:         conv,
		logger:       log,
		revProxy:     revProxy,
		client:       client,
		streamClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		jobs:         newJobManager(cfg.ResultTTL, cfg.MaxKeptJobs),
		admission:    newAdmission(tempFree),
		downloadSem:  make(chan struct{}, max(cfg.DownloadConcurrency, 2)),
		baseCtx:      baseCtx,
		cancelBase:   cancel,
	}
	s.server = &http.Server{
		Addr:              cfg.BindAddr,
		Handler:           s,
		ReadHeaderTimeout: 30 * time.Second,
		// No ReadTimeout/WriteTimeout: big uploads and long waits must work.
	}
	return s
}

// Start binds the listener synchronously (so a busy port is a startup error)
// and serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.BindAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.BindAddr, err)
	}
	s.listenerMu.Lock()
	s.listener = ln
	s.listenerMu.Unlock()
	s.logger.Printf("Starting reverse proxy server on %s -> %s", ln.Addr(), s.cfg.UpstreamURL.String())
	if s.cfg.PassUploads {
		s.logger.Printf("Uploads are passed to Immich as they are (proxy_convert_uploads=false): tag them %q to convert them in library mode", "immich-optimizer/convert")
	}
	go s.jobs.janitor(s.baseCtx)
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Errorf("Reverse proxy server error: %v", err)
		}
	}()
	return nil
}

// Addr returns the bound address once started.
func (s *Server) Addr() string {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

// BeginShutdown stops accepting connections and new jobs. Handlers waiting
// on jobs keep their connections until the jobs finish or Close is called.
func (s *Server) BeginShutdown() {
	s.logger.Printf("Stopping reverse proxy server...")
	s.lifecycle.Lock()
	s.shuttingDown = true
	s.lifecycle.Unlock()
	go func() { _ = s.server.Shutdown(context.Background()) }()
}

// Drain waits for running jobs until ctx is done.
func (s *Server) Drain(ctx context.Context) error {
	s.lifecycle.Lock()
	s.shuttingDown = true
	s.lifecycle.Unlock()
	done := make(chan struct{})
	go func() {
		s.jobsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cancel aborts running jobs (conversion processes are killed). Clients get
// 503 with Retry-After.
func (s *Server) Cancel() {
	s.cancelBase(entity.ErrShuttingDown)
}

// FinishShutdown waits up to ctx for active handlers to write the results of
// jobs that have already finished, then force-closes whatever is left. Call
// it after Drain (or Cancel and Drain) so a job that completed during the
// grace period still reaches its client.
func (s *Server) FinishShutdown(ctx context.Context) error {
	s.lifecycle.Lock()
	s.shuttingDown = true
	s.lifecycle.Unlock()
	err := s.server.Shutdown(ctx)
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close force-closes remaining connections.
func (s *Server) Close() error {
	s.cancelBase(entity.ErrShuttingDown)
	return s.server.Close()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == WaitPath {
		s.continueJob(w, r)
		return
	}
	if r.URL.Path == HealthPath {
		s.lifecycle.Lock()
		stopping := s.shuttingDown
		s.lifecycle.Unlock()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if stopping {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "shutting down\n")
			return
		}
		_, _ = io.WriteString(w, "ok\n")
		return
	}

	if s.serveAppSync(w, r) {
		return
	}
	if s.serveDownload(w, r) {
		return
	}

	if !s.isUploadRequest(r) || s.cfg.PassUploads {
		r.Host = s.cfg.UpstreamURL.Host
		s.revProxy.ServeHTTP(w, r)
		return
	}

	s.logger.Printf("Intercepted upload request from %s: %s", r.RemoteAddr, r.URL.Path)
	async := s.clientFollowsRedirects(r)

	// Before reading the body: is there room for it? If not, the client
	// waits here, which slows it down to what the conversions keep up with.
	place := s.admission.acquire(r.Context(), r.ContentLength)
	if place == nil {
		if r.Context().Err() == nil {
			s.logger.Warnf("The temp folder has no room for the upload from %s next to the ones in progress; asked the client to retry", r.RemoteAddr)
			w.Header().Set("Retry-After", admissionRetryAfter)
			http.Error(w, "busy converting earlier uploads, retry later", http.StatusServiceUnavailable)
		}
		return
	}

	// Phase A (bound to the client): spool the body to disk.
	spoolPath, err := spoolBody(r)
	if err != nil {
		place.release()
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, errSpoolCreate) {
			s.logger.Errorf("Unable to buffer upload from %s: %v", r.RemoteAddr, err)
			w.Header().Set("Retry-After", "60")
			http.Error(w, "temporarily unable to buffer upload", http.StatusServiceUnavailable)
			return
		}
		s.logger.Printf("Incomplete upload from %s, not processed: %v", r.RemoteAddr, err)
		http.Error(w, "failed to read upload body", http.StatusBadRequest)
		return
	}

	// Phase B (detached): process and upload.
	if st, err := os.Stat(spoolPath); err == nil {
		place.resize(st.Size())
	}
	j, ok := s.startJob(newJobSpec(r, spoolPath, place), async)
	if !ok {
		place.release()
		_ = os.Remove(spoolPath)
		w.Header().Set("Retry-After", "30")
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}

	if async {
		http.Redirect(w, r, fmt.Sprintf("%s?job=%s", WaitPath, j.id), http.StatusTemporaryRedirect)
		return
	}

	select {
	case <-j.done:
		j.result.write(w)
	case <-r.Context().Done():
		s.logger.Printf("Client %s disconnected; job %s continues in background", r.RemoteAddr, j.id)
	}
}

func (s *Server) startJob(spec jobSpec, async bool) (*job, bool) {
	s.lifecycle.Lock()
	if s.shuttingDown {
		s.lifecycle.Unlock()
		return nil, false
	}
	s.jobsWG.Add(1)
	s.lifecycle.Unlock()

	j := s.jobs.create(async)
	go func() {
		defer s.jobsWG.Done()
		// Last of all: the place is free once every file of the job is gone.
		defer spec.place.release()
		defer os.Remove(spec.spoolPath)
		var res jobResult
		func() {
			defer func() {
				if p := recover(); p != nil {
					s.logger.Errorf("Proxy job %s panicked: %v", j.id, p)
					res = textResult(http.StatusInternalServerError, "internal error, view logs for more info", false)
				}
			}()
			res = s.runJob(s.baseCtx, j, spec)
		}()
		s.jobs.finish(j, res)
	}()
	return j, true
}

func (s *Server) clientFollowsRedirects(r *http.Request) bool {
	acceptHeader := r.Header.Get("Accept")
	follows := strings.Contains(acceptHeader, "text/html")

	brokenRedirectUserAgents := []string{"Dart/", "Dalvik/", "Immich"}
	for _, userAgent := range brokenRedirectUserAgents {
		if strings.HasPrefix(r.UserAgent(), userAgent) {
			return false
		}
	}
	return follows
}

func (s *Server) continueJob(w http.ResponseWriter, r *http.Request) {
	// The browser's 307 re-sends the whole upload; discard it.
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
	}

	jobID := r.URL.Query().Get("job")
	if jobID == "" {
		http.Error(w, "missing job parameter", http.StatusBadRequest)
		return
	}
	j := s.jobs.get(jobID)
	if j == nil {
		http.Error(w, "job not found or expired", http.StatusNotFound)
		return
	}

	t := time.NewTimer(s.cfg.WaitPoll)
	defer t.Stop()
	select {
	case <-j.done:
		j.result.write(w)
	case <-t.C:
		http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
	case <-r.Context().Done():
	}
}

func (s *Server) isUploadRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		return false
	}

	pathLower := strings.ToLower(r.URL.Path)
	return strings.HasSuffix(pathLower, "/api/assets") ||
		strings.HasSuffix(pathLower, "/api/assets/") ||
		strings.HasSuffix(pathLower, "/api/asset/upload") ||
		strings.HasSuffix(pathLower, "/api/asset/upload/") ||
		strings.Contains(pathLower, "/assets/upload")
}
