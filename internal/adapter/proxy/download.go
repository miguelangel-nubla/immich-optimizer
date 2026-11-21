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
// including archives of several assets and the edited version of an asset,
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
	name string
	at   time.Time
}

const nameCacheTTL = time.Minute

func (c *nameCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.names[key]
	if !ok || time.Since(n.at) > nameCacheTTL {
		return "", false
	}
	return n.name, true
}

func (c *nameCache) put(key, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.names == nil || len(c.names) > 10000 {
		c.names = map[string]cachedName{}
	}
	c.names[key] = cachedName{name, time.Now()}
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
// that may be converted.
func originalID(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", false
	}
	p := r.URL.Path
	if !strings.HasPrefix(p, originalPathPrefix) || !strings.HasSuffix(p, originalPathSuffix) {
		return "", false
	}
	id := p[len(originalPathPrefix) : len(p)-len(originalPathSuffix)]
	if !isUUID(id) {
		return "", false
	}
	// The edited version is rendered by Immich, in a format of its choice.
	if v := r.URL.Query().Get("edited"); v != "" && v != "false" {
		return "", false
	}
	return id, true
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

// assetName asks Immich for the file name of an asset.
func (s *Server) assetName(r *http.Request, id string) (string, bool) {
	key := id + "\x00" + r.Header.Get("Authorization") + "\x00" + r.Header.Get("X-Api-Key") + "\x00" + r.Header.Get("Cookie") + "\x00" + r.URL.RawQuery
	if name, ok := s.names.get(key); ok {
		return name, true
	}
	resp, err := s.upstreamGet(r.Context(), r, originalPathPrefix+id, s.client)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var a struct {
		Name string `json:"originalFileName"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&a) != nil || a.Name == "" {
		return "", false
	}
	s.names.put(key, a.Name)
	return a.Name, true
}

// serveDownload answers the download of an original with its converted form
// when a download task handles the asset's type. It reports whether it
// answered; when it did not, the request goes to Immich untouched.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) bool {
	if len(s.cfg.Downloads) == 0 || s.cfg.Processor == nil {
		return false
	}
	id, ok := originalID(r)
	if !ok {
		return false
	}
	name, ok := s.assetName(r, id)
	if !ok || !s.downloadTask(name) {
		return false
	}

	// The whole file is needed to convert it, whatever part was asked for.
	resp, err := s.upstreamGet(r.Context(), r, r.URL.Path, s.streamClient)
	if err != nil {
		if r.Context().Err() == nil {
			s.logger.Errorf("Download of %s: could not reach Immich: %v", name, err)
		}
		http.Error(w, "failed to reach Immich", http.StatusBadGateway)
		return true
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Immich's own answer (not found, not allowed), as it is.
		for k, vv := range resp.Header {
			w.Header()[k] = vv
		}
		for _, k := range hopHeaders {
			w.Header().Del(k)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return true
	}

	work, err := os.MkdirTemp("", "io-download-*")
	if err != nil {
		s.logger.Errorf("Download of %s: %v", name, err)
		http.Error(w, "temporarily unable to prepare the download", http.StatusServiceUnavailable)
		return true
	}
	defer os.RemoveAll(work)
	ext := filepath.Ext(name)
	if strings.ContainsAny(ext, `/\*`) || len(ext) > 32 {
		ext = ""
	}
	src := filepath.Join(work, "original"+ext)
	if err := spoolTo(src, resp.Body); err != nil {
		if r.Context().Err() == nil {
			s.logger.Errorf("Download of %s: %v", name, err)
		}
		http.Error(w, "failed to read the file from Immich", http.StatusBadGateway)
		return true
	}

	outPath, outName := src, name
	res, err := s.convertDownload(entity.WithTaskInfo(r.Context(), entity.TaskInfo{OriginalName: name, Source: entity.SourceDownload}), src)
	if res != nil && res.Cleanup != nil {
		defer res.Cleanup()
	}
	switch {
	case r.Context().Err() != nil:
		return true
	case err != nil || res == nil || res.ProcessedFilePath == "" || res.ProcessedSize == 0:
		// The stored file is what the person asked for in the first place:
		// better that than nothing.
		s.logger.Warnf("Download of %s: the conversion failed, sending the stored file: %v", name, err)
	default:
		outPath = res.ProcessedFilePath
		outExt := filepath.Ext(res.ProcessedFilename)
		if outExt == "" {
			outExt = filepath.Ext(res.ProcessedFilePath)
		}
		outName = utils.TrimSuffixCaseInsensitive(name, ext) + outExt
		s.logger.Debugf("Download of %s: sent as %s (task %s)", name, outName, res.Task)
	}

	f, err := os.Open(outPath) // #nosec G304 -- a file this request just wrote
	if err != nil {
		s.logger.Errorf("Download of %s: %v", name, err)
		http.Error(w, "failed to prepare the download", http.StatusInternalServerError)
		return true
	}
	defer f.Close()

	h := w.Header()
	if v := resp.Header.Get("Cache-Control"); v != "" {
		h.Set("Cache-Control", v)
	}
	disposition := "attachment"
	if d, _, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil && d != "" {
		disposition = d
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": outName}))
	ctype := resp.Header.Get("Content-Type")
	if outPath != src {
		ctype = mime.TypeByExtension(strings.ToLower(filepath.Ext(outName)))
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h.Set("Content-Type", ctype)
	// ServeContent answers the part that was asked for, and HEAD.
	http.ServeContent(w, r, "", time.Time{}, f)
	return true
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

// convertDownload runs the download tasks on a file, a few at a time.
func (s *Server) convertDownload(ctx context.Context, path string) (*entity.ProcessResult, error) {
	select {
	case s.downloadSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.downloadSem }()
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
