package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// maxUpstreamBody caps the buffered upstream response. Immich upload
// responses are about 100 bytes.
const maxUpstreamBody = 1 << 20

const msgProcessFailed = "failed to process upload, view logs for more info"

// jobResult is a fully resolved HTTP response, so it can be served to any
// number of waiting clients (sync, async /wait, coalesced retries).
type jobResult struct {
	status int
	header http.Header
	body   []byte
	// cacheable results are served to identical retries within the TTL.
	cacheable bool
	err       error
}

func (r jobResult) write(w http.ResponseWriter) {
	for k, vv := range r.header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body)
}

func textResult(status int, msg string, cacheable bool) jobResult {
	h := make(http.Header)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	return jobResult{status: status, header: h, body: []byte(msg + "\n"), cacheable: cacheable}
}

// errBadRequest marks malformed multipart input.
var errBadRequest = errors.New("bad request")

// errUpstream marks transport failures talking to Immich.
var errUpstream = errors.New("upstream request failed")

// resultFromError maps a job error to the HTTP response sent to the client.
func resultFromError(err error) jobResult {
	var ce *entity.ConversionError
	var res jobResult
	switch {
	case errors.Is(err, entity.ErrCanceled) || errors.Is(err, entity.ErrShuttingDown):
		res = textResult(http.StatusServiceUnavailable, "server is shutting down, retry later", false)
		res.header.Set("Retry-After", "30")
	case errors.Is(err, entity.ErrAuditUnavailable):
		res = textResult(http.StatusServiceUnavailable, "the optimizer cannot write its audit log, retry later", false)
		res.header.Set("Retry-After", "60")
	case errors.As(err, &ce) && ce.Failure.Stage == entity.StageNoTask:
		// Not cacheable if saving to undone failed: a retry must try again.
		res = textResult(http.StatusUnsupportedMediaType,
			fmt.Sprintf("blocked unmatched file extension: %s", filepath.Ext(ce.Filename)), ce.UndoneErr == nil)
	case errors.As(err, &ce):
		// Only cache when the original is safely in undone; otherwise the
		// original exists nowhere and an identical retry must be processed
		// (and saved) again.
		res = textResult(http.StatusBadGateway, msgProcessFailed, ce.UndoneErr == nil && ce.Undone != nil)
	case errors.Is(err, errBadRequest):
		res = textResult(http.StatusBadRequest, "malformed multipart upload", false)
	case errors.Is(err, errUpstream):
		res = textResult(http.StatusBadGateway, "failed to upload to Immich, view logs for more info", false)
	default:
		res = textResult(http.StatusBadGateway, msgProcessFailed, false)
	}
	res.err = err
	return res
}
