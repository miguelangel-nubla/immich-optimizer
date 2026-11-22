package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/immich"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

// Converting on the way out.
//
// What is stored in Immich may be in a format the person downloading it
// cannot open (JPEG XL is the usual case). The "downloads" list of the tasks
// file says how to turn such a file into something else when its original is
// downloaded. Nothing in Immich changes: the conversion is made for that one
// download and thrown away.
//
// Only the download of a single original is converted. Everything else,
// including archives of several assets and the version of an asset edited in
// Immich,
// passes through as Immich sends it.

// originalPathPrefix and originalPathSuffix frame the id in the path of an
// original's download.
const (
	originalPathPrefix = "/api/assets/"
	originalPathSuffix = "/original"
)

// nameCache remembers the file names of assets for a short while. A player
// asks for one original many times, in pieces.
type nameCache struct {
	mu    sync.Mutex
	names map[string]cachedName
}

type cachedName struct {
	name   string
	edited bool
	at     time.Time
}

const nameCacheTTL = time.Minute

func (c *nameCache) get(key string) (cachedName, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.names[key]
	if !ok || time.Since(n.at) > nameCacheTTL {
		return cachedName{}, false
	}
	return n, true
}

func (c *nameCache) put(key string, n cachedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil || len(c.names) > 10000 {
		c.names = map[string]cachedName{}
	}
	n.at = time.Now()
	c.names[key] = n
}

// downloadTask reports whether some download task handles the file name.
func (s *Server) downloadTask(name string) bool {
	ext := utils.NormalizeExtension(filepath.Ext(name))
	for _, t := range s.cfg.Downloads {
		for _, e := range t.Extensions {
			if utils.NormalizeExtension(e) == ext {
				return true
			}
		}
	}
	return false
}

// originalID returns the asset id when the request downloads an original
// that may be converted, and whether it asks for the edited version.
func originalID(r *http.Request) (id string, edited, ok bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", false, false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, originalPathPrefix) || !strings.HasSuffix(p, originalPathSuffix) {
		return "", false, false
	}
	id = p[len(originalPathPrefix) : len(p)-len(originalPathSuffix)]
	if !isUUID(id) {
		return "", false, false
	}
	// The apps and the web ask for "edited" on every download: Immich then
	// sends the edited version when the asset has one, and the original
	// otherwise.
	v := r.URL.Query().Get("edited")
	return id, v != "" && v != "false", true
}

// upstreamGet asks Immich for path with the caller's credentials and query
// (a shared link's key is in the query).
func (s *Server) upstreamGet(ctx context.Context, r *http.Request, path string, client *http.Client) (*http.Response, error) {
	u := *s.cfg.UpstreamURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = immich.AuthHeaders(r.Header)
	return client.Do(req)
}

// assetName asks Immich for the file name of an asset, and whether it has
// been edited in Immich.
func (s *Server) assetName(r *http.Request, id string) (cachedName, bool) {
	key := id + "\x00" + r.Header.Get("Authorization") + "\x00" + r.Header.Get("X-Api-Key") + "\x00" + r.Header.Get("Cookie") + "\x00" + r.URL.RawQuery
	if n, ok := s.names.get(key); ok {
		return n, true
	}
	resp, err := s.upstreamGet(r.Context(), r, originalPathPrefix+id, s.client)
	if err != nil {
		return cachedName{}, false
	}
	defer resp.Body.Close()
	var a struct {
		Name   string `json:"originalFileName"`
		Edited bool   `json:"isEdited"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&a) != nil || a.Name == "" {
		return cachedName{}, false
	}
	n := cachedName{name: a.Name, edited: a.Edited}
	s.names.put(key, n)
	return n, true
}

// serveDownload answers the download of an original with its converted form
// when a download task handles the asset's type. It reports whether it
// answered; when it did not, the request goes to Immich untouched.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) bool {
	if len(s.cfg.Downloads) == 0 || s.cfg.Processor == nil || s.cfg.PassDownloads {
		return false
	}
	id, wantsEdited, ok := originalID(r)
	if !ok {
		return false
	}
	asset, ok := s.assetName(r, id)
	// An edited version is rendered by Immich, in a format of its choice.
	if !ok || wantsEdited && asset.edited || !s.downloadTask(asset.name) {
		return false
	}
	name := asset.name

	// The whole file is needed to convert it, whatever part was asked for.
	// A player or a download manager asks for one file many times, in
	// pieces: they all share one conversion, kept for a short while.
	e := s.downloads.acquire(s.downloadKey(r, id), func(ctx context.Context, e *preparedDownload) {
		s.prepareDownload(ctx, e, r.Clone(ctx), name)
	})
	defer s.downloads.release(e)
	select {
	case <-e.done:
	case <-r.Context().Done():
		return true
	}
	switch {
	case e.pass:
		// Immich did not send the file (not found, not allowed): it gives
		// that answer itself.
		return false
	case e.err != nil:
		if r.Context().Err() == nil {
			s.logger.Errorf("Download of %s: %v", name, e.err)
		}
		http.Error(w, "failed to prepare the download", http.StatusBadGateway)
		return true
	}

	f, err := os.Open(e.path) // #nosec G304 -- a file the proxy itself wrote
	if err != nil {
		s.logger.Errorf("Download of %s: %v", name, err)
		http.Error(w, "failed to prepare the download", http.StatusInternalServerError)
		return true
	}
	defer f.Close()

	h := w.Header()
	if e.cacheControl != "" {
		h.Set("Cache-Control", e.cacheControl)
	}
	h.Set("Content-Disposition", mime.FormatMediaType(e.disposition, map[string]string{"filename": e.name}))
	h.Set("Content-Type", e.ctype)
	// ServeContent answers the part that was asked for, and HEAD.
	http.ServeContent(w, r, "", time.Time{}, f)
	return true
}

// downloadKey names one asset as one caller may download it.
func (s *Server) downloadKey(r *http.Request, id string) string {
	var key strings.Builder
	key.WriteString(id)
	auth := immich.AuthHeaders(r.Header)
	names := make([]string, 0, len(auth))
	for k := range auth {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		key.WriteString("\x00" + k + "\x00" + strings.Join(auth[k], "\x00"))
	}
	key.WriteString("\x00" + r.URL.RawQuery)
	return key.String()
}

// preparedDownload is one original, fetched from Immich and converted, ready
// to be sent. Its fields are set by prepareDownload and read after done.
type preparedDownload struct {
	key    string
	done   chan struct{}
	cancel context.CancelFunc

	// waiters counts the requests waiting for it or sending it; last is
	// when the latest of them left. Both belong to the cache's lock.
	waiters int
	last    time.Time

	dir, path, name, ctype    string
	cacheControl, disposition string
	// pass means Immich did not send the file.
	pass bool
	err  error
}

// usable reports whether the download can be sent. Only read after done.
func (e *preparedDownload) usable() bool { return e.err == nil && !e.pass }

// downloadCacheTTL is how long a prepared download is kept after the last
// request for it; maxPreparedDownloads how many are kept at most.
const (
	downloadCacheTTL     = time.Minute
	maxPreparedDownloads = 16
)

// downloadCache holds the prepared downloads. It is temporary files and
// nothing else: emptied at shutdown, and of no consequence when lost.
type downloadCache struct {
	mu      sync.Mutex
	entries map[string]*preparedDownload
	base    context.Context
}

// acquire returns the prepared download for key, starting its preparation
// when there is none. The caller must release it.
func (c *downloadCache) acquire(key string, prepare func(context.Context, *preparedDownload)) *preparedDownload {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(time.Now())
	if e := c.entries[key]; e != nil {
		select {
		case <-e.done:
			if e.usable() {
				e.waiters++
				return e
			}
		default:
			e.waiters++
			return e
		}
	}
	base := c.base
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	e := &preparedDownload{key: key, done: make(chan struct{}), cancel: cancel, waiters: 1}
	if c.entries == nil {
		c.entries = map[string]*preparedDownload{}
	}
	c.entries[key] = e
	go func() {
		defer cancel()
		prepare(ctx, e)
		// Nothing to send, or nobody left to send it to (it was dropped
		// while it was being prepared): its files go. Decided and marked
		// done under the lock, so that drop sees one or the other.
		c.mu.Lock()
		if c.entries[key] != e && e.err == nil {
			e.err = context.Canceled
		}
		if !e.usable() && e.dir != "" {
			_ = os.RemoveAll(e.dir)
			e.dir = ""
		}
		close(e.done)
		c.mu.Unlock()
	}()
	return e
}

// release ends one request's use of a prepared download.
func (c *downloadCache) release(e *preparedDownload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.waiters--
	e.last = time.Now()
	if e.waiters > 0 {
		return
	}
	select {
	case <-e.done:
		if !e.usable() {
			c.drop(e)
			return
		}
		// Kept for the requests that follow; removed when none does.
		time.AfterFunc(downloadCacheTTL+time.Second, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.sweep(time.Now())
		})
	default:
		// Nobody waits for it any more: stop preparing it.
		e.cancel()
		c.drop(e)
	}
}

// drop forgets an entry and removes its files. The lock is held.
func (c *downloadCache) drop(e *preparedDownload) {
	if c.entries[e.key] == e {
		delete(c.entries, e.key)
	}
	select {
	case <-e.done:
		if e.dir != "" {
			_ = os.RemoveAll(e.dir)
		}
	default:
		// Still being prepared, and cancelled: it removes its own files.
	}
}

// sweep drops the entries nobody used for a while, and the least recently
// used ones beyond the limit. The lock is held.
func (c *downloadCache) sweep(now time.Time) {
	var idle []*preparedDownload
	for _, e := range c.entries {
		if e.waiters > 0 {
			continue
		}
		if now.Sub(e.last) > downloadCacheTTL {
			c.drop(e)
			continue
		}
		idle = append(idle, e)
	}
	if over := len(c.entries) - maxPreparedDownloads; over > 0 {
		sort.Slice(idle, func(i, j int) bool { return idle[i].last.Before(idle[j].last) })
		for _, e := range idle[:min(over, len(idle))] {
			c.drop(e)
		}
	}
}

// clear removes everything that is not in use, at shutdown.
func (c *downloadCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.waiters == 0 {
			c.drop(e)
		}
	}
}

// prepareDownload fetches an original from Immich and converts it. Only a
// few run at the same time, the fetch included: each holds a whole file.
func (s *Server) prepareDownload(ctx context.Context, e *preparedDownload, r *http.Request, name string) {
	select {
	case s.downloadSem <- struct{}{}:
	case <-ctx.Done():
		e.err = ctx.Err()
		return
	}
	defer func() { <-s.downloadSem }()

	resp, err := s.upstreamGet(ctx, r, r.URL.Path, s.streamClient)
	if err != nil {
		e.err = fmt.Errorf("could not reach Immich: %w", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.pass = true
		return
	}

	if e.dir, err = os.MkdirTemp("", "io-download-*"); err != nil {
		e.err = err
		return
	}
	ext := filepath.Ext(name)
	if strings.ContainsAny(ext, `/\*`) || len(ext) > 32 {
		ext = ""
	}
	src := filepath.Join(e.dir, "original"+ext)
	if err := spoolTo(src, resp.Body); err != nil {
		e.err = fmt.Errorf("reading the file from Immich: %w", err)
		return
	}

	e.path, e.name = src, name
	e.ctype = resp.Header.Get("Content-Type")
	res, err := s.convertDownload(entity.WithTaskInfo(ctx, entity.TaskInfo{OriginalName: name, Source: entity.SourceDownload}), src)
	switch {
	case ctx.Err() != nil:
		if res != nil && res.Cleanup != nil {
			res.Cleanup()
		}
		e.err = ctx.Err()
		return
	case err != nil || res == nil || res.ProcessedFilePath == "" || res.ProcessedSize == 0:
		// The stored file is what the person asked for in the first place:
		// better that than nothing.
		s.logger.Warnf("Download of %s: the conversion failed, sending the stored file: %v", name, err)
		if res != nil && res.Cleanup != nil {
			res.Cleanup()
		}
	default:
		outExt := filepath.Ext(res.ProcessedFilename)
		if outExt == "" {
			outExt = filepath.Ext(res.ProcessedFilePath)
		}
		// Into the download's own folder: the task's work folder goes.
		out := filepath.Join(e.dir, "converted"+outExt)
		err := os.Rename(res.ProcessedFilePath, out)
		if err != nil {
			err = copyFile(res.ProcessedFilePath, out)
		}
		if res.Cleanup != nil {
			res.Cleanup()
		}
		if err != nil {
			e.err = err
			return
		}
		_ = os.Remove(src)
		e.path = out
		e.name = utils.TrimSuffixCaseInsensitive(name, ext) + outExt
		e.ctype = mime.TypeByExtension(strings.ToLower(outExt))
		s.logger.Debugf("Download of %s: sent as %s (task %s)", name, e.name, res.Task)
	}
	if e.ctype == "" {
		e.ctype = "application/octet-stream"
	}
	e.cacheControl = resp.Header.Get("Cache-Control")
	e.disposition = "attachment"
	if d, _, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && d != "" {
		e.disposition = d
	}
}

func copyFile(from, to string) error {
	in, err := os.Open(from) // #nosec G304 -- a file a task of this download wrote
	if err != nil {
		return err
	}
	defer in.Close()
	return spoolTo(to, in)
}

func spoolTo(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- inside a folder made for this request
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// convertDownload runs the download tasks on a file.
func (s *Server) convertDownload(ctx context.Context, path string) (*entity.ProcessResult, error) {
	if s.cfg.DownloadTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.DownloadTimeout)
		defer cancel()
	}
	res, err := s.cfg.Processor.Process(ctx, path, s.cfg.Downloads)
	if err != nil {
		return res, fmt.Errorf("download task: %w", err)
	}
	return res, nil
}
