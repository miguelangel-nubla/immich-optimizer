package proxy

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type jobResult struct {
	resp *http.Response
	err  error
}

type jobState struct {
	respCh chan jobResult
	doneCh chan struct{}
}

type Server struct {
	upstreamURL   *url.URL
	bindAddr      string
	filterPath    string
	filterFormKey string
	processor     port.MediaProcessor
	tasks         []entity.Task
	logger        *logger.Logger
	proxy         *httputil.ReverseProxy
	server        *http.Server
	semaphore     chan struct{}
	client        *http.Client

	jobsMu sync.Mutex
	jobs   map[string]*jobState
}

func NewServer(
	upstreamURL *url.URL,
	bindAddr string,
	filterPath string,
	filterFormKey string,
	processor port.MediaProcessor,
	tasks []entity.Task,
	log *logger.Logger,
	maxConcurrent int,
	httpTimeout int,
) *Server {
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	if filterPath == "" {
		filterPath = "/api/assets"
	}
	if filterFormKey == "" {
		filterFormKey = "assetData"
	}

	revProxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	timeout := 120 * time.Second
	if httpTimeout > 0 {
		timeout = time.Duration(httpTimeout) * time.Second
	}

	s := &Server{
		upstreamURL:   upstreamURL,
		bindAddr:      bindAddr,
		filterPath:    filterPath,
		filterFormKey: filterFormKey,
		processor:     processor,
		tasks:         tasks,
		logger:        log,
		proxy:         revProxy,
		semaphore:     make(chan struct{}, maxConcurrent),
		client: &http.Client{
			Timeout: timeout,
		},
		jobs: make(map[string]*jobState),
	}

	s.server = &http.Server{
		Addr:    bindAddr,
		Handler: s,
	}

	return s
}

func (s *Server) Start() error {
	s.logger.Printf("Starting reverse proxy server on %s -> %s", s.bindAddr, s.upstreamURL.String())
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("Reverse proxy server error: %v", err)
		}
	}()
	return nil
}

func (s *Server) Stop(ctx context.Context) error {
	s.logger.Printf("Stopping reverse proxy server...")
	return s.server.Shutdown(ctx)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_immich-upload-optimizer/wait" {
		s.continueJob(w, r)
		return
	}

	if !s.isUploadRequest(r) {
		r.Host = s.upstreamURL.Host
		s.proxy.ServeHTTP(w, r)
		return
	}

	s.logger.Printf("Intercepted upload request from %s: %s", r.RemoteAddr, r.URL.Path)

	if s.clientFollowsRedirects(r) {
		s.newAsyncJob(w, r)
		return
	}

	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	case <-r.Context().Done():
		http.Error(w, "Request context canceled", http.StatusGatewayTimeout)
		return
	}

	resp, err := s.handleUploadProxy(r)
	if err != nil {
		s.logger.Printf("Proxy upload error: %v; falling back to direct proxy", err)
		r.Host = s.upstreamURL.Host
		s.proxy.ServeHTTP(w, r)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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

func (s *Server) newAsyncJob(w http.ResponseWriter, r *http.Request) {
	jobID := generateUUID()
	job := &jobState{
		respCh: make(chan jobResult, 1),
		doneCh: make(chan struct{}, 1),
	}

	s.jobsMu.Lock()
	s.jobs[jobID] = job
	s.jobsMu.Unlock()

	http.Redirect(w, r, fmt.Sprintf("/_immich-upload-optimizer/wait?job=%s", jobID), http.StatusTemporaryRedirect)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	go func() {
		defer func() {
			s.jobsMu.Lock()
			delete(s.jobs, jobID)
			s.jobsMu.Unlock()
		}()

		select {
		case s.semaphore <- struct{}{}:
			defer func() { <-s.semaphore }()
		case <-r.Context().Done():
			job.respCh <- jobResult{err: r.Context().Err()}
			return
		}

		resp, err := s.handleUploadProxy(r)
		job.respCh <- jobResult{resp: resp, err: err}

		if err == nil && resp != nil {
			select {
			case <-job.doneCh:
			case <-time.After(10 * time.Second):
			}
		}
	}()
}

func (s *Server) continueJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job")
	if jobID == "" {
		http.Error(w, "missing job parameter", http.StatusBadRequest)
		return
	}

	s.jobsMu.Lock()
	job, exists := s.jobs[jobID]
	s.jobsMu.Unlock()

	if !exists {
		http.Error(w, "job not found", http.StatusBadRequest)
		return
	}

	_ = r.ParseMultipartForm(10 << 20)

	safeTimeout := 55 * time.Second

	select {
	case res, ok := <-job.respCh:
		if !ok || res.err != nil {
			msg := "job failed or channel closed"
			if res.err != nil {
				msg = fmt.Sprintf("job failed: %v", res.err)
			}
			http.Error(w, msg, http.StatusInternalServerError)
			return
		}

		defer res.resp.Body.Close()

		for k, vv := range res.resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(res.resp.StatusCode)
		_, _ = io.Copy(w, res.resp.Body)

		select {
		case job.doneCh <- struct{}{}:
		default:
		}
	case <-time.After(safeTimeout):
		http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
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

	pathStr := r.URL.Path
	matched, err := path.Match(s.filterPath, pathStr)
	if err == nil && matched {
		return true
	}

	pathLower := strings.ToLower(pathStr)
	return strings.HasSuffix(pathLower, "/api/assets") ||
		strings.HasSuffix(pathLower, "/api/assets/") ||
		strings.HasSuffix(pathLower, "/api/asset/upload") ||
		strings.HasSuffix(pathLower, "/api/asset/upload/") ||
		strings.Contains(pathLower, "/assets/upload")
}

func (s *Server) shouldOptimizeExt(checkExt string) bool {
	checkExt = utils.NormalizeExtension(checkExt)
	for _, task := range s.tasks {
		for _, ext := range task.Extensions {
			if utils.NormalizeExtension(ext) == checkExt {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleUploadProxy(r *http.Request) (*http.Response, error) {
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return nil, fmt.Errorf("invalid multipart media type: %w", err)
	}
	boundary, ok := params["boundary"]
	if !ok {
		return nil, fmt.Errorf("missing multipart boundary")
	}

	mr := multipart.NewReader(r.Body, boundary)

	outgoingFile, err := os.CreateTemp("", "proxy-out-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp output file: %w", err)
	}
	defer func() {
		outgoingFile.Close()
		os.Remove(outgoingFile.Name())
	}()

	mw := multipart.NewWriter(outgoingFile)

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("error reading multipart part: %w", err)
		}

		filename := part.FileName()
		formname := part.FormName()

		shouldTarget := (s.filterFormKey == "" || formname == s.filterFormKey)

		if filename != "" && shouldTarget && s.shouldOptimizeExt(filepath.Ext(filename)) {
			if err := s.processFilePart(r.Context(), part, mw, formname, filename); err != nil {
				s.logger.Printf("Error optimizing file part %s: %v; forwarding original", filename, err)
			}
		} else {
			h := part.Header
			if h == nil {
				h = make(textproto.MIMEHeader)
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, formname, filename))
			}
			pw, createErr := mw.CreatePart(h)
			if createErr != nil {
				return nil, fmt.Errorf("failed to create part in multipart writer: %w", createErr)
			}
			if _, copyErr := io.Copy(pw, part); copyErr != nil {
				return nil, fmt.Errorf("failed to copy part: %w", copyErr)
			}
		}
	}

	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	if _, err := outgoingFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek output file: %w", err)
	}

	stat, err := outgoingFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat output file: %w", err)
	}

	targetURL := s.upstreamURL.ResolveReference(r.URL)
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), outgoingFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create outgoing request: %w", err)
	}

	for k, vv := range r.Header {
		if strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vv {
			outReq.Header.Add(k, v)
		}
	}

	outReq.Host = s.upstreamURL.Host
	outReq.Header.Set("Content-Type", mw.FormDataContentType())
	outReq.ContentLength = stat.Size()

	return s.client.Do(outReq)
}

func (s *Server) processFilePart(ctx context.Context, part *multipart.Part, mw *multipart.Writer, formname, filename string) error {
	ext := filepath.Ext(filename)
	tempSrc, err := os.CreateTemp("", "proxy-src-*"+ext)
	if err != nil {
		return fmt.Errorf("failed to create temp src file: %w", err)
	}
	defer func() {
		tempSrc.Close()
		os.Remove(tempSrc.Name())
	}()

	if _, err := io.Copy(tempSrc, part); err != nil {
		return fmt.Errorf("failed to copy part to temp src file: %w", err)
	}
	tempSrc.Close()

	res, procErr := s.processor.Process(ctx, tempSrc.Name(), s.tasks)
	if procErr == nil && res != nil {
		if res.Cleanup != nil {
			defer res.Cleanup()
		}
		if res.ProcessedSize < res.OriginalSize && res.ProcessedFilePath != "" {
			s.logger.Printf("Proxy optimized asset %s: %s -> %s",
				filename,
				utils.HumanReadableSize(res.OriginalSize),
				utils.HumanReadableSize(res.ProcessedSize))

			procFile, openErr := os.Open(res.ProcessedFilePath)
			if openErr == nil {
				defer procFile.Close()
				h := make(textproto.MIMEHeader)
				outFilename := res.ProcessedFilename
				if outFilename == "" {
					outFilename = filename
				}
				h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, formname, outFilename))
				if part.Header != nil && part.Header.Get("Content-Type") != "" {
					h.Set("Content-Type", part.Header.Get("Content-Type"))
				}
				pw, createErr := mw.CreatePart(h)
				if createErr == nil {
					_, copyErr := io.Copy(pw, procFile)
					return copyErr
				}
			}
		}
	}

	s.logger.Printf("File %s not optimized (no reduction or processing failed), forwarding original", filename)
	origFile, openErr := os.Open(tempSrc.Name())
	if openErr != nil {
		return fmt.Errorf("failed to re-open temp src file: %w", openErr)
	}
	defer origFile.Close()

	h := part.Header
	if h == nil {
		h = make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, formname, filename))
	}
	pw, createErr := mw.CreatePart(h)
	if createErr != nil {
		return fmt.Errorf("failed to create part for original file: %w", createErr)
	}
	_, copyErr := io.Copy(pw, origFile)
	return copyErr
}

func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
