package proxy

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- content identity matching Immich's checksum, not security
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/immich"
	"github.com/miguelangel-nubla/immich-optimizer/internal/audit"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

var errSpoolCreate = errors.New("unable to create upload spool file")

// spoolBody copies the request body to a temp file. This is the only phase
// bound to the client connection: an aborted or short body never reaches the
// engine and is never saved to undone, because the input is incomplete.
func spoolBody(r *http.Request) (string, error) {
	f, err := os.CreateTemp("", "io-proxy-body-*")
	if err != nil {
		return "", fmt.Errorf("%w: %w", errSpoolCreate, err)
	}
	_, err = io.Copy(f, r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// targetPart is a file part handed to the conversion engine.
type targetPart struct {
	index       int
	formName    string
	fileName    string // base name of the client's multipart filename
	fileDir     string // directory part of the client's filename, if it sent one
	contentType string
	srcPath     string
	sha1        string // lowercase hex SHA-1 of the content, as Immich computes it
	res         *entity.ConvertResult
	// named says the name came from the form's name field, which then has
	// to be given the converted file's name too.
	named bool
}

// smallFields are non-file form fields read for diagnostics and mtime.
var smallFields = map[string]bool{"fileModifiedAt": true, "fileCreatedAt": true, "deviceId": true, nameField: true}

// nameField is a form field with the asset's file name. Immich prefers it to
// the name on the file part, and the mobile app's background upload sends
// it: there the file part carries the name of a temporary export (on iOS
// something like "59DC...A630_L0_001_1773008161.884566_o_IMG_0992.MOV").
const nameField = "filename"

// uploadFormKey is the form field Immich's upload carries the file in.
const uploadFormKey = "assetData"

func (s *Server) isTarget(part *multipart.Part) bool {
	return part.FileName() != "" && part.FormName() == uploadFormKey
}

// runJob processes one spooled upload on the detached server context.
func (s *Server) runJob(ctx context.Context, j *job, spec jobSpec) jobResult {
	_, params, err := mime.ParseMediaType(spec.header.Get("Content-Type"))
	boundary := params["boundary"]
	if err != nil || boundary == "" {
		return resultFromError(fmt.Errorf("%w: invalid multipart content type", errBadRequest))
	}

	// Pass 1: extract target file parts (hashing them) and small fields.
	targets, fields, formHash, err := s.extractTargets(spec.spoolPath, boundary)
	releaseTargets := func() {
		for _, t := range targets {
			if t.res != nil {
				t.res.Release()
			}
			_ = os.Remove(t.srcPath)
		}
		targets = nil
	}
	defer func() { releaseTargets() }()
	if err != nil {
		s.logger.Printf("Proxy job %s: %v", j.id, err)
		return resultFromError(err)
	}

	// Coalesce identical uploads from the same user (single-file requests).
	// The key covers the file content, every other form field (e.g.
	// livePhotoVideoId, visibility), the request path and query and the
	// caller's identity, so only true retries share a result.
	if len(targets) == 1 {
		key := coalesceKey(targets[0].sha1, formHash, spec)
		existing, dup := s.jobs.attach(key, j)
		if dup && s.resultIsStale(ctx, existing, spec) {
			// The asset that upload created has since been deleted from
			// Immich: its answer would send the client away with nothing
			// stored. This upload is handled as a new one.
			s.logger.Printf("Proxy job %s: the asset of the identical upload handled by job %s is gone from Immich, uploading again", j.id, existing.id)
			existing, dup = s.jobs.replace(key, existing, j)
			dup = !dup
		}
		if dup {
			s.logger.Printf("Proxy job %s: identical upload of %s already handled by job %s, reusing its result",
				j.id, targets[0].fileName, existing.id)
			// This job's copies are never used: free the disk now instead of
			// holding them for as long as the original job runs.
			releaseTargets()
			_ = os.Remove(spec.spoolPath)
			// Nor its place in the temp folder: a client that retries while
			// a long conversion runs would otherwise hold back other uploads
			// with room it does not use. (Releasing again later is a no-op.)
			spec.place.release()
			select {
			case <-existing.done:
				res := existing.result
				res.cacheable = false
				return res
			case <-ctx.Done():
				return resultFromError(fmt.Errorf("%w: %w", entity.ErrCanceled, context.Cause(ctx)))
			}
		}
	}

	// The name Immich will use is the one to convert under and to rename.
	if name := fields[nameField]; name != "" && len(targets) == 1 {
		if _, base := splitClientFilename(name); base != "" {
			targets[0].fileName, targets[0].named = base, true
		}
	}

	modTime := parseTime(fields["fileModifiedAt"])
	for _, t := range targets {
		in := entity.ConvertInput{
			Path:        t.srcPath,
			Filename:    t.fileName,
			OriginalDir: t.fileDir,
			Source:      entity.SourceProxy,
			ModTime:     modTime,
			SHA1:        t.sha1,
			Details: map[string]string{
				"remote_addr": spec.remoteAddr,
				"user_agent":  spec.userAgent,
				"device_id":   fields["deviceId"],
			},
		}
		res, err := s.conv.Convert(ctx, in)
		if err != nil {
			return resultFromError(err)
		}
		t.res = res
	}

	// Pass 2: build the outgoing multipart in the original part order.
	outPath, contentType, err := s.buildOutgoing(spec.spoolPath, boundary, targets)
	if outPath != "" {
		defer os.Remove(outPath)
	}
	if err != nil {
		s.logger.Errorf("Proxy job %s: %v", j.id, err)
		return resultFromError(err)
	}
	_ = os.Remove(spec.spoolPath) // no longer needed; frees disk during the upload

	res := s.upload(ctx, spec, outPath, contentType)
	if res.err != nil {
		s.logger.Errorf("Proxy job %s: upload to Immich failed: %v", j.id, res.err)
	} else {
		s.logger.Printf("Proxy job %s: Immich responded %d", j.id, res.status)
		s.markDuplicate(ctx, j, spec, targets, res)
	}
	s.auditUpload(targets, res)
	return res
}

// extractTargets copies target file parts to temp files and reads the small
// diagnostic fields. formHash digests every part except the target files'
// content (names, filenames, content types and all other values, in order),
// so uploads that differ in any form field never coalesce.
func (s *Server) extractTargets(spoolPath, boundary string) (targets []*targetPart, fields map[string]string, formHash string, err error) {
	f, err := os.Open(spoolPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open spool: %w", err)
	}
	defer f.Close()

	fields = map[string]string{}
	form := sha256.New()
	mr := multipart.NewReader(f, boundary)
	for idx := 0; ; idx++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return targets, fields, "", fmt.Errorf("%w: reading multipart part: %w", errBadRequest, err)
		}
		ph := sha256.New()
		writeField(ph, part.FormName())
		writeField(ph, rawFileName(part))
		writeField(ph, part.Header.Get("Content-Type"))
		if s.isTarget(part) {
			t, err := extractPart(part, idx)
			if t != nil {
				targets = append(targets, t)
			}
			if err != nil {
				return targets, fields, "", err
			}
			_, _ = form.Write(ph.Sum([]byte("file")))
			continue
		}
		var small bytes.Buffer
		var r io.Reader = part
		if part.FileName() == "" && smallFields[part.FormName()] {
			r = io.MultiReader(io.TeeReader(io.LimitReader(part, 1024), &small), part)
		}
		if _, err := io.Copy(ph, r); err != nil {
			return targets, fields, "", fmt.Errorf("%w: reading multipart field %s: %w", errBadRequest, part.FormName(), err)
		}
		if small.Len() > 0 {
			fields[part.FormName()] = strings.TrimSpace(small.String())
		}
		_, _ = form.Write(ph.Sum([]byte("field")))
	}
	return targets, fields, hex.EncodeToString(form.Sum(nil)), nil
}

// writeField writes a length-prefixed value so field boundaries are
// unambiguous in a digest.
func writeField(w io.Writer, v string) {
	_, _ = fmt.Fprintf(w, "%d:%s", len(v), v)
}

// rawFileName returns the filename exactly as the client sent it in the
// Content-Disposition header (multipart.Part.FileName strips directories).
func rawFileName(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return part.FileName()
	}
	return params["filename"]
}

// splitClientFilename splits a client filename into its directory and base
// name on '/' only. The base equals what multipart.Part.FileName returns, so
// the name delivered to Immich is unchanged; a backslash is a legal filename
// character on the clients and is kept. The directory is informational.
func splitClientFilename(raw string) (dir, base string) {
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		dir, base = raw[:i], raw[i+1:]
	} else {
		base = raw
	}
	if base == "" || base == "." || base == ".." {
		base = filepath.Base(raw)
	}
	return dir, base
}

func extractPart(part *multipart.Part, idx int) (*targetPart, error) {
	// Deliver exactly the name Go's multipart parser reports (as before the
	// engine refactor); only the directory part is taken from the raw value.
	dir, _ := splitClientFilename(rawFileName(part))
	name := part.FileName()
	ext := filepath.Ext(name)
	if strings.ContainsAny(ext, `/\*`) || len(ext) > 32 {
		ext = ""
	}
	src, err := os.CreateTemp("", "io-proxy-src-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("create temp src file: %w", err)
	}
	t := &targetPart{
		index:       idx,
		formName:    part.FormName(),
		fileName:    name,
		fileDir:     dir,
		contentType: part.Header.Get("Content-Type"),
		srcPath:     src.Name(),
	}
	h := sha1.New() // #nosec G401 -- content identity, see targetPart.sha1
	_, err = io.Copy(io.MultiWriter(src, h), part)
	if cerr := src.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return t, fmt.Errorf("%w: copying file part %s: %w", errBadRequest, t.fileName, err)
	}
	t.sha1 = hex.EncodeToString(h.Sum(nil))
	return t, nil
}

// resultIsStale reports whether a finished job's answer names an asset that
// Immich no longer has. A job that is still running, an answer without an
// asset, and any doubt (Immich unreachable, an unexpected status) all count
// as not stale.
func (s *Server) resultIsStale(ctx context.Context, existing *job, spec jobSpec) bool {
	select {
	case <-existing.done:
	default:
		return false
	}
	var answer struct {
		ID string `json:"id"`
	}
	res := existing.result
	if res.status < 200 || res.status > 299 || json.Unmarshal(res.body, &answer) != nil || answer.ID == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	target := strings.TrimRight(s.cfg.UpstreamURL.String(), "/") + "/api/assets/" + url.PathEscape(answer.ID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	req.Header = immich.AuthHeaders(spec.header)
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	// Immich answers 400 for an asset that does not exist.
	return resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound
}

// markerOf returns the provenance marker of the request's converted file. A
// request with several converted files gets none: one asset, one marker.
func markerOf(targets []*targetPart) *entity.Marker {
	var m *entity.Marker
	for _, t := range targets {
		if t.res != nil && t.res.Marker != nil {
			if m != nil {
				return nil
			}
			m = t.res.Marker
		}
	}
	return m
}

// markDuplicate records the marker on the existing asset when Immich answered
// "duplicate": in that case it ignored the metadata sent with the upload.
func (s *Server) markDuplicate(ctx context.Context, j *job, spec jobSpec, targets []*targetPart, res jobResult) {
	marker := markerOf(targets)
	if marker == nil || res.status != http.StatusOK {
		return
	}
	id := immich.DuplicateAssetID(res.body)
	if id == "" {
		return
	}
	err := immich.EnsureMarker(ctx, s.client, s.cfg.UpstreamURL.String(), immich.AuthHeaders(spec.header), id, marker, true)
	if err != nil {
		s.logger.Warnf("Proxy job %s: could not record the optimizer marker on existing asset %s: %v", j.id, id, err)
	}
}

// auditUpload records what became of the converted file(s) of one request.
func (s *Server) auditUpload(targets []*targetPart, res jobResult) {
	if s.cfg.Audit == nil {
		return
	}
	var answer struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	ok := res.err == nil && (res.status == http.StatusOK || res.status == http.StatusCreated) &&
		json.Unmarshal(res.body, &answer) == nil && answer.ID != ""
	for _, t := range targets {
		if t.res == nil {
			continue
		}
		var entry audit.Entry
		if ok {
			entry = audit.Delivered(entity.SourceProxy, t.fileName, t.res, entity.UploadResult{AssetID: answer.ID, Duplicate: answer.Status == "duplicate"})
		} else {
			entry = audit.Delivered(entity.SourceProxy, t.fileName, t.res, entity.UploadResult{})
			entry.Event = "upload_failed"
			if res.err != nil {
				entry.Detail = res.err.Error()
			} else {
				entry.Detail = fmt.Sprintf("Immich answered %d", res.status)
			}
		}
		if err := s.cfg.Audit.Write(entry); err != nil {
			s.logger.Warnf("Could not write the audit log: %v", err)
		}
	}
}

// maxMetadataField bounds the client's "metadata" form field that is parsed
// to merge the marker into it.
const maxMetadataField = 1 << 20

func (s *Server) buildOutgoing(spoolPath, boundary string, targets []*targetPart) (string, string, error) {
	byIndex := make(map[int]*targetPart, len(targets))
	for _, t := range targets {
		byIndex[t.index] = t
	}
	marker := markerOf(targets)

	in, err := os.Open(spoolPath)
	if err != nil {
		return "", "", fmt.Errorf("open spool: %w", err)
	}
	defer in.Close()

	out, err := os.CreateTemp("", "io-proxy-out-*")
	if err != nil {
		return "", "", fmt.Errorf("create outgoing temp file: %w", err)
	}
	outPath := out.Name()
	fail := func(err error) (string, string, error) {
		_ = out.Close()
		return outPath, "", err
	}

	mw := multipart.NewWriter(out)
	mr := multipart.NewReader(in, boundary)
	for idx := 0; ; idx++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("%w: re-reading multipart part: %w", errBadRequest, err))
		}
		if t := byIndex[idx]; t != nil {
			if err := writeDelivered(mw, t); err != nil {
				return fail(err)
			}
			continue
		}
		pw, err := mw.CreatePart(part.Header)
		if err != nil {
			return fail(fmt.Errorf("create multipart part: %w", err))
		}
		// Immich takes the asset's name from this field: it must be the
		// converted file's name, or a JXL would be stored as "IMG_1.HEIC".
		if part.FileName() == "" && part.FormName() == nameField && len(targets) == 1 && targets[0].named {
			if _, err := io.WriteString(pw, targets[0].res.DeliverName); err != nil {
				return fail(fmt.Errorf("write name field: %w", err))
			}
			continue
		}
		// The client's own asset metadata (e.g. the mobile app's item): add
		// the marker to it. Immich rejects the same key twice, so the merge
		// replaces any marker the client sent.
		if marker != nil && part.FormName() == "metadata" && part.FileName() == "" {
			raw, err := io.ReadAll(io.LimitReader(part, maxMetadataField+1))
			if err != nil {
				return fail(fmt.Errorf("%w: reading metadata field: %w", errBadRequest, err))
			}
			merged, mergeErr := marker.MergeIntoMetadata(strings.TrimSpace(string(raw)))
			if len(raw) > maxMetadataField || mergeErr != nil {
				// Not ours to fix: forward it untouched, without a marker.
				s.logger.Warnf("Not adding the optimizer marker: client metadata field could not be merged (%v)", mergeErr)
				if _, err := io.Copy(pw, io.MultiReader(bytes.NewReader(raw), part)); err != nil {
					return fail(fmt.Errorf("copy multipart part: %w", err))
				}
			} else if _, err := io.WriteString(pw, merged); err != nil {
				return fail(fmt.Errorf("write metadata field: %w", err))
			}
			marker = nil
			continue
		}
		if _, err := io.Copy(pw, part); err != nil {
			return fail(fmt.Errorf("copy multipart part: %w", err))
		}
	}
	if marker != nil {
		merged, err := marker.MergeIntoMetadata("")
		if err != nil {
			return fail(fmt.Errorf("encode marker: %w", err))
		}
		if err := mw.WriteField("metadata", merged); err != nil {
			return fail(fmt.Errorf("write metadata field: %w", err))
		}
	}
	if err := mw.Close(); err != nil {
		return fail(fmt.Errorf("close multipart writer: %w", err))
	}
	if err := out.Close(); err != nil {
		return outPath, "", fmt.Errorf("close outgoing temp file: %w", err)
	}
	return outPath, mw.FormDataContentType(), nil
}

// writeDelivered writes the converted file as a part. Any failure here is a
// job error: the original is never forwarded as a fallback.
func writeDelivered(mw *multipart.Writer, t *targetPart) error {
	f, err := os.Open(t.res.DeliverPath)
	if err != nil {
		return fmt.Errorf("open converted file for %s: %w", t.fileName, err)
	}
	defer f.Close()

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
		escapeQuotes(t.formName), escapeQuotes(t.res.DeliverName)))
	ct := t.contentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	pw, err := mw.CreatePart(h)
	if err != nil {
		return fmt.Errorf("create part for converted file %s: %w", t.fileName, err)
	}
	if _, err := io.Copy(pw, f); err != nil {
		return fmt.Errorf("copy converted file %s: %w", t.fileName, err)
	}
	return nil
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "%0D", "\n", "%0A")

func escapeQuotes(s string) string { return quoteEscaper.Replace(s) }

var hopHeaders = []string{"Content-Length", "Connection", "Keep-Alive", "Transfer-Encoding", "Proxy-Connection", "Upgrade", "Te", "Trailer"}

// upstreamTarget is the address in Immich for a request's path and query.
// Only those two are taken from the request: a client can put a full address
// in its request line, and that one must never be the one contacted.
func (s *Server) upstreamTarget(u *url.URL) string {
	t := *s.cfg.UpstreamURL
	// The escaped form is joined, so that an escaped "/" in the request's
	// path stays one.
	raw := strings.TrimRight(t.EscapedPath(), "/") + "/" + strings.TrimLeft(u.EscapedPath(), "/")
	if p, err := url.PathUnescape(raw); err == nil {
		t.Path, t.RawPath = p, raw
	} else {
		t.Path, t.RawPath = strings.TrimRight(t.Path, "/")+"/"+strings.TrimLeft(u.Path, "/"), ""
	}
	t.RawQuery = u.RawQuery
	t.Fragment = ""
	return t.String()
}

func (s *Server) upload(ctx context.Context, spec jobSpec, outPath, contentType string) jobResult {
	f, err := os.Open(outPath)
	if err != nil {
		return resultFromError(fmt.Errorf("open outgoing file: %w", err))
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return resultFromError(fmt.Errorf("stat outgoing file: %w", err))
	}

	req, err := http.NewRequestWithContext(ctx, spec.method, s.upstreamTarget(spec.url), f)
	if err != nil {
		return resultFromError(fmt.Errorf("create outgoing request: %w", err))
	}
	for k, vv := range spec.header {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Type", "Content-Length", "Expect":
			continue
		case "Accept-Encoding":
			// The answer is read here. Without the client's own
			// Accept-Encoding, Go's transport handles compression itself
			// and hands over plain text.
			continue
		case "X-Immich-Checksum":
			// The client's checksum of the file it holds. Immich uses it
			// to answer "duplicate" without reading the body, which is
			// not that file any more once it has been converted.
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	for _, k := range hopHeaders {
		req.Header.Del(k)
	}
	req.Host = s.cfg.UpstreamURL.Host
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = st.Size()

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return resultFromError(fmt.Errorf("%w: %w", entity.ErrCanceled, context.Cause(ctx)))
		}
		return resultFromError(fmt.Errorf("%w: %w", errUpstream, err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return resultFromError(fmt.Errorf("%w: %w", entity.ErrCanceled, context.Cause(ctx)))
		}
		return resultFromError(fmt.Errorf("%w: reading response: %w", errUpstream, err))
	}
	if len(body) > maxUpstreamBody {
		return resultFromError(fmt.Errorf("%w: response body larger than %d bytes", errUpstream, maxUpstreamBody))
	}

	h := resp.Header.Clone()
	for _, k := range hopHeaders {
		h.Del(k)
	}
	// The answer is kept and may be given to another request of the same
	// user (a retry, a browser waiting): a cookie is not to be handed on.
	h.Del("Set-Cookie")
	return jobResult{
		status:    resp.StatusCode,
		header:    h,
		body:      body,
		cacheable: cacheableStatus(resp.StatusCode),
	}
}

// cacheableStatus reports whether an Immich answer is definitive enough to be
// replayed to identical retries. 5xx and transient 4xx answers (often from a
// reverse proxy in front of Immich) must reach Immich again on retry.
func cacheableStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusRequestEntityTooLarge, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return code >= 200 && code < 500
}

// coalesceKey identifies an upload for in-flight and recent-result
// deduplication: file content, all other form fields, method, path, query
// and the caller's credentials. Credentials are only ever hashed.
// fileSHA1 is the content's SHA-1 (as Immich computes it); formHash and the
// auth fingerprint are internal SHA-256 digests.
func coalesceKey(fileSHA1, formHash string, spec jobSpec) string {
	return "v3|" + fileSHA1 + "|" + formHash + "|" + authFingerprint(spec)
}

// authFingerprint identifies the caller and the request target so coalesced
// results never leak across users or endpoints. It is hashed, never stored
// or logged in clear.
func authFingerprint(spec jobSpec) string {
	var b bytes.Buffer
	b.WriteString(spec.method)
	b.WriteByte(0)
	b.WriteString(spec.url.Path)
	b.WriteByte(0)
	b.WriteString(spec.url.RawQuery)
	for _, k := range []string{"Authorization", "X-Api-Key", "X-Immich-User-Token", "X-Immich-Session-Token", "X-Immich-Share-Key", "Cookie"} {
		b.WriteByte(0)
		b.WriteString(strings.Join(spec.header.Values(k), ","))
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:])
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.000Z0700", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t
		}
	}
	return time.Time{}
}
