package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/immich"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// Experimental: making the Immich mobile app see converted assets as backed
// up.
//
// The app decides that a photo is backed up when its own database, filled
// from Immich's sync stream, has an asset with the checksum of the file on
// the phone. A converted asset has another checksum, so the app uploads the
// photo again on every backup run.
//
// Every converted asset carries a marker with the checksum of its original,
// and that marker travels in the sync stream too. When one passes through,
// the proxy adds one extra line to the stream: a hidden asset entry that has
// the original's checksum. The app then finds its match.
//
// Nothing is stored anywhere: the hidden entry is derived from the marker
// line itself, and its id from the real asset's id.
//
//   - The hidden entry is flagged as belonging to an external library. The app
//     only allows one asset per checksum outside libraries, so it can coexist
//     with an original that is still on the server.
//   - Its visibility is "hidden", so the app never shows it.
//   - It is never marked deleted: the app can be set to delete phone photos
//     whose server asset is in the trash.
//   - When the real asset is deleted, or loses its marker, the hidden entry is
//     deleted from the phone as well.

const (
	syncStreamPath = "/api/sync/stream"
	syncAckPath    = "/api/sync/ack"

	// hiddenAckPrefix marks the acknowledgement tokens of lines the proxy
	// added. The app sends every token back to the server; these are removed
	// on the way, because Immich has never heard of them.
	hiddenAckPrefix = "io-hidden|"

	// hiddenLibraryID is the made-up library the hidden entries belong to.
	hiddenLibraryID = "696d6d69-6368-4f70-8074-696d697a6572"
	hiddenFileName  = "immich-optimizer-original"
	hiddenDate      = "2000-01-01T00:00:00.000Z"
)

// uuidVariantIndex is the position of the variant digit in a UUID string.
// Real UUIDs have 8, 9, a or b there; hidden ids have c, d, e or f, which no
// UUID generator produces, so the two can never be confused and each can be
// computed from the other.
const uuidVariantIndex = 19

func isUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' {
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

var variantSwap = map[byte]byte{'8': 'c', '9': 'd', 'a': 'e', 'b': 'f', 'A': 'e', 'B': 'f'}
var variantBack = map[byte]byte{'c': '8', 'd': '9', 'e': 'a', 'f': 'b', 'C': '8', 'D': '9', 'E': 'a', 'F': 'b'}

// hiddenID returns the id of the hidden entry of an asset, or "" when the
// asset id is not an ordinary UUID.
func hiddenID(assetID string) string {
	if !isUUID(assetID) {
		return ""
	}
	v, ok := variantSwap[assetID[uuidVariantIndex]]
	if !ok {
		return ""
	}
	return assetID[:uuidVariantIndex] + string(v) + assetID[uuidVariantIndex+1:]
}

// realID returns the asset a hidden id stands for.
func realID(id string) (string, bool) {
	if !isUUID(id) {
		return "", false
	}
	v, ok := variantBack[id[uuidVariantIndex]]
	if !ok {
		return "", false
	}
	return id[:uuidVariantIndex] + string(v) + id[uuidVariantIndex+1:], true
}

// rewriteHiddenIDs replaces hidden ids in a request path by the real ones, so
// anything the app asks about a hidden entry is answered with the real asset.
func rewriteHiddenIDs(path string) (string, bool) {
	parts := strings.Split(path, "/")
	changed := false
	for i, p := range parts {
		if real, ok := realID(p); ok {
			parts[i] = real
			changed = true
		}
	}
	if !changed {
		return path, false
	}
	return strings.Join(parts, "/"), true
}

type syncLine struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	Ack  string          `json:"ack"`
}

// syncInjector adds the hidden entries to one sync stream response.
type syncInjector struct {
	// assetType is the asset line type the client asked for ("AssetV1" or
	// "AssetV2"), or "" when it asked for no assets.
	assetType string
	// ownerID returns the id of the user the stream belongs to.
	ownerID func() (string, error)

	owner    string
	ownerErr error
	asked    bool

	// Injected counts the hidden entries added.
	Injected int
}

func (in *syncInjector) owned() (string, error) {
	if !in.asked {
		in.asked = true
		in.owner, in.ownerErr = in.ownerID()
	}
	return in.owner, in.ownerErr
}

func encodeLine(l syncLine) []byte {
	b, _ := json.Marshal(l)
	return append(b, '\n')
}

// extra returns the lines to add after one line of the stream.
func (in *syncInjector) extra(line []byte) [][]byte {
	// Cheap test first: almost every line is of no interest.
	if in.assetType == "" || !(bytes.Contains(line, []byte(`"AssetMetadataV1"`)) ||
		bytes.Contains(line, []byte(`"AssetMetadataDeleteV1"`)) || bytes.Contains(line, []byte(`"AssetDeleteV1"`))) {
		return nil
	}
	var l syncLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}
	del := func(assetID, ack string) [][]byte {
		id := hiddenID(assetID)
		if id == "" {
			return nil
		}
		data, _ := json.Marshal(map[string]string{"assetId": id})
		return [][]byte{encodeLine(syncLine{Type: "AssetDeleteV1", Data: data, Ack: ack})}
	}
	switch l.Type {
	case "AssetDeleteV1":
		var d struct {
			AssetID string `json:"assetId"`
		}
		if json.Unmarshal(l.Data, &d) != nil {
			return nil
		}
		// Same kind of line as the one before it, so it carries the same
		// token: the app acknowledges only the last line of a run.
		return del(d.AssetID, l.Ack)

	case "AssetMetadataDeleteV1":
		var d struct {
			AssetID string `json:"assetId"`
			Key     string `json:"key"`
		}
		if json.Unmarshal(l.Data, &d) != nil || d.Key != entity.MarkerKey {
			return nil
		}
		return del(d.AssetID, hiddenAckPrefix+d.AssetID)

	case "AssetMetadataV1":
		var d struct {
			AssetID string        `json:"assetId"`
			Key     string        `json:"key"`
			Value   entity.Marker `json:"value"`
		}
		if json.Unmarshal(l.Data, &d) != nil || d.Key != entity.MarkerKey {
			return nil
		}
		m := d.Value
		sum, err := hex.DecodeString(m.SHA1)
		id := hiddenID(d.AssetID)
		// Not for an original that was kept as it is (its own checksum is
		// already right), nor for a replacement that is not finished.
		if err != nil || len(sum) != 20 || id == "" || m.Kept || m.State == entity.MarkerStatePending {
			return nil
		}
		owner, err := in.owned()
		if err != nil || owner == "" {
			return nil
		}
		data, _ := json.Marshal(map[string]any{
			"id":               id,
			"ownerId":          owner,
			"originalFileName": hiddenFileName,
			"thumbhash":        nil,
			"checksum":         base64.StdEncoding.EncodeToString(sum),
			"fileCreatedAt":    hiddenDate,
			"fileModifiedAt":   hiddenDate,
			"createdAt":        hiddenDate,
			"localDateTime":    hiddenDate,
			"duration":         nil,
			"type":             "IMAGE",
			"deletedAt":        nil,
			"isFavorite":       false,
			"visibility":       "hidden",
			"livePhotoVideoId": nil,
			"stackId":          nil,
			"libraryId":        hiddenLibraryID,
			"width":            nil,
			"height":           nil,
			"isEdited":         false,
		})
		in.Injected++
		return [][]byte{encodeLine(syncLine{Type: in.assetType, Data: data, Ack: hiddenAckPrefix + d.AssetID})}
	}
	return nil
}

// copyStream copies a sync stream from r to w line by line, adding the hidden
// entries. flush is called after every chunk read from r, so the client gets
// lines as soon as the server sends them.
func (in *syncInjector) copyStream(w io.Writer, r io.Reader, flush func()) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				line = append(line, '\n')
			}
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
			for _, extra := range in.extra(line) {
				if _, werr := w.Write(extra); werr != nil {
					return werr
				}
			}
			if br.Buffered() == 0 {
				flush()
			}
		}
		if err != nil {
			flush()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// assetLineType says which asset line type a sync request asked for.
func assetLineType(body []byte) string {
	var req struct {
		Types []string `json:"types"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	t := ""
	for _, x := range req.Types {
		switch x {
		case "AssetsV2":
			return "AssetV2"
		case "AssetsV1":
			t = "AssetV1"
		}
	}
	return t
}

// userCache remembers which user a set of credentials belongs to. It is only
// a shortcut: when empty it is filled again by asking Immich.
type userCache struct {
	mu  sync.Mutex
	ids map[string]string
}

func (c *userCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.ids[key]
	return id, ok
}

func (c *userCache) put(key, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ids == nil || len(c.ids) > 10000 {
		c.ids = map[string]string{}
	}
	c.ids[key] = id
}

// userID asks Immich who the request's credentials belong to.
func (s *Server) userID(ctx context.Context, h http.Header) (string, error) {
	auth := immich.AuthHeaders(h)
	var key bytes.Buffer
	for _, k := range []string{"Authorization", "X-Api-Key", "X-Immich-User-Token", "X-Immich-Session-Token", "Cookie"} {
		key.WriteString(strings.Join(auth.Values(k), ","))
		key.WriteByte(0)
	}
	if id, ok := s.users.get(key.String()); ok {
		return id, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.cfg.UpstreamURL.String(), "/")+"/api/users/me", nil)
	if err != nil {
		return "", err
	}
	req.Header = auth
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var me struct {
		ID string `json:"id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&me) != nil || me.ID == "" {
		return "", fmt.Errorf("asking Immich for the current user: status %d", resp.StatusCode)
	}
	s.users.put(key.String(), me.ID)
	return me.ID, nil
}

// serveSyncStream proxies the app's sync request and adds the hidden entries
// to the answer.
func (s *Server) serveSyncStream(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read request", http.StatusBadRequest)
		return
	}
	target := s.cfg.UpstreamURL.ResolveReference(r.URL)
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k, vv := range r.Header {
		// The answer is edited line by line. Without the client's own
		// Accept-Encoding, Go's transport handles compression itself and
		// hands over plain text.
		if strings.EqualFold(k, "Accept-Encoding") || strings.EqualFold(k, "Content-Length") {
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

	resp, err := s.streamClient.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			s.logger.Errorf("Sync request to Immich failed: %v", err)
		}
		http.Error(w, "failed to reach Immich", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	h := w.Header()
	for k, vv := range resp.Header {
		h[k] = vv
	}
	for _, k := range hopHeaders {
		h.Del(k)
	}
	h.Del("Content-Length")
	h.Del("Content-Encoding")
	w.WriteHeader(resp.StatusCode)
	flush := func() {}
	if f, ok := w.(http.Flusher); ok {
		flush = f.Flush
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(w, resp.Body)
		return
	}

	in := &syncInjector{
		assetType: assetLineType(body),
		ownerID:   func() (string, error) { return s.userID(r.Context(), r.Header) },
	}
	if err := in.copyStream(w, resp.Body, flush); err != nil && r.Context().Err() == nil {
		s.logger.Warnf("Sync stream ended early: %v", err)
	}
	if in.ownerErr != nil {
		s.logger.Warnf("App sync: no hidden entries added to this sync: %v", in.ownerErr)
	}
	if in.Injected > 0 {
		s.logger.Debugf("App sync: added %d hidden entr(ies) for converted assets", in.Injected)
	}
}

// serveSyncAck removes the tokens of proxy-made lines from an
// acknowledgement before it reaches Immich. It reports whether it answered
// the request itself.
func (s *Server) serveSyncAck(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "failed to read request", http.StatusBadRequest)
		return true
	}
	restore := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", fmt.Sprint(len(b)))
	}
	var req struct {
		Acks []string `json:"acks"`
	}
	if json.Unmarshal(body, &req) != nil {
		restore(body)
		return false
	}
	kept := make([]string, 0, len(req.Acks))
	for _, a := range req.Acks {
		if !strings.HasPrefix(a, hiddenAckPrefix) {
			kept = append(kept, a)
		}
	}
	if len(kept) == len(req.Acks) {
		restore(body)
		return false
	}
	if len(kept) == 0 {
		// Only proxy-made tokens: there is nothing to tell Immich.
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	out, _ := json.Marshal(map[string][]string{"acks": kept})
	restore(out)
	return false
}

// serveAppSync handles the requests the app sync feature cares about. It
// reports whether the request was answered.
func (s *Server) serveAppSync(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.AppSync {
		return false
	}
	if p, changed := rewriteHiddenIDs(r.URL.Path); changed {
		r.URL.Path, r.URL.RawPath = p, ""
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == syncStreamPath:
		s.serveSyncStream(w, r)
		return true
	case r.Method == http.MethodPost && r.URL.Path == syncAckPath:
		return s.serveSyncAck(w, r)
	}
	return false
}
