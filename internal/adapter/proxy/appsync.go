package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/immich"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// Making the Immich mobile app see converted assets as backed up.
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
//   - It is in the trash exactly when the real asset is, like the entry of an
//     ordinary asset would be: the app does not offer to free up a phone photo
//     whose server copy is in the trash. Trashing and restoring do not touch
//     the marker, so for those the proxy asks Immich for the marker of the
//     asset whose line goes by.
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

	// marker returns the marker of an asset, or nil. It is asked for assets
	// whose own line goes by, which is how a change that does not touch the
	// marker (trash, restore) reaches the hidden entry.
	marker func(assetID string) *entity.Marker

	// trashed remembers, for this one stream, the assets that are in the
	// trash and since when; done the hidden entries already sent.
	trashed map[string]string
	done    map[string]bool
	lookups int

	// cloud holds, for this one stream, the iOS app's own record of each
	// asset it was told about (its iCloud id), and cloudSent the hidden
	// entries that were given theirs.
	cloud     map[string]json.RawMessage
	cloudSent map[string]bool

	// Injected counts the hidden entries added.
	Injected int
}

// mobileAppKey is the asset metadata key under which the iOS app keeps a
// photo's iCloud id.
//
// The app wants every server asset that matches one of its photos to have
// that record, and keeps sending it for the ones that have none. A hidden
// entry matches a photo, so it gets a copy of the real asset's record: the
// app then leaves it alone.
const mobileAppKey = "mobile-app"

// maxCloudRecords bounds how many of those records one stream remembers.
const maxCloudRecords = 50000

// maxLookups bounds how many markers are asked for in one stream for assets
// that are not in the trash. A first sync has a line for every asset, and
// each of them gets its hidden entry from its marker line anyway.
const maxLookups = 200

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
		bytes.Contains(line, []byte(`"AssetMetadataDeleteV1"`)) || bytes.Contains(line, []byte(`"AssetDeleteV1"`)) ||
		bytes.Contains(line, []byte(`"`+in.assetType+`"`))) {
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
		if json.Unmarshal(l.Data, &d) != nil {
			return nil
		}
		if d.Key == mobileAppKey {
			return in.cloudRecord(l.Data)
		}
		if d.Key != entity.MarkerKey || in.done[d.AssetID] {
			return nil
		}
		return in.entry(d.AssetID, &d.Value, hiddenAckPrefix+d.AssetID)

	case in.assetType:
		var d struct {
			ID        string  `json:"id"`
			DeletedAt *string `json:"deletedAt"`
		}
		if json.Unmarshal(l.Data, &d) != nil || hiddenID(d.ID) == "" || in.marker == nil {
			return nil
		}
		if d.DeletedAt != nil {
			if in.trashed == nil {
				in.trashed = map[string]string{}
			}
			in.trashed[d.ID] = *d.DeletedAt
		} else {
			if in.lookups >= maxLookups {
				return nil
			}
			in.lookups++
		}
		// Same kind of line as the one before it, so the same token.
		return in.entry(d.ID, in.marker(d.ID), l.Ack)
	}
	return nil
}

// entry returns the hidden entry line of an asset with marker m, or nil when
// it gets none.
func (in *syncInjector) entry(assetID string, m *entity.Marker, ack string) [][]byte {
	sum, ok := hiddenChecksum(m)
	id := hiddenID(assetID)
	if !ok || id == "" {
		return nil
	}
	owner, err := in.owned()
	if err != nil || owner == "" {
		return nil
	}
	asset := hiddenAsset(id, owner, sum)
	if since, ok := in.trashed[assetID]; ok {
		asset["deletedAt"] = since
	}
	data, _ := json.Marshal(asset)
	if in.done == nil {
		in.done = map[string]bool{}
	}
	in.done[assetID] = true
	in.Injected++
	lines := [][]byte{encodeLine(syncLine{Type: in.assetType, Data: data, Ack: ack})}
	return append(lines, in.cloudLine(assetID)...)
}

// cloudRecord handles the app's own record of an asset going by. When the
// asset has a hidden entry, the entry gets a copy.
func (in *syncInjector) cloudRecord(data json.RawMessage) [][]byte {
	var d struct {
		AssetID string          `json:"assetId"`
		Value   json.RawMessage `json:"value"`
	}
	if json.Unmarshal(data, &d) != nil || hiddenID(d.AssetID) == "" || len(d.Value) == 0 {
		return nil
	}
	if in.cloud == nil {
		in.cloud = map[string]json.RawMessage{}
	}
	if len(in.cloud) < maxCloudRecords {
		in.cloud[d.AssetID] = d.Value
	}
	if in.done[d.AssetID] {
		return in.cloudLine(d.AssetID)
	}
	// The hidden entry was not sent in this stream. It may have been in an
	// earlier one, or may follow with the marker (which then brings the
	// record along). Ask, within the limit: with a marker, the entry is
	// sent again so that the record certainly has something to belong to.
	if in.marker == nil || in.lookups >= maxLookups {
		return nil
	}
	in.lookups++
	return in.entry(d.AssetID, in.marker(d.AssetID), hiddenAckPrefix+d.AssetID)
}

// cloudLine returns the record line for the hidden entry of an asset, once.
func (in *syncInjector) cloudLine(assetID string) [][]byte {
	value, ok := in.cloud[assetID]
	if !ok || in.cloudSent[assetID] {
		return nil
	}
	if in.cloudSent == nil {
		in.cloudSent = map[string]bool{}
	}
	in.cloudSent[assetID] = true
	data, _ := json.Marshal(map[string]any{"assetId": hiddenID(assetID), "key": mobileAppKey, "value": value})
	return [][]byte{encodeLine(syncLine{Type: "AssetMetadataV1", Data: data, Ack: hiddenAckPrefix + "cloud|" + assetID})}
}

// hiddenAsset is the asset entry the app gets for the original of a converted
// asset.
func hiddenAsset(id, owner string, sum []byte) map[string]any {
	return map[string]any{
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
	}
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
		marker: func(assetID string) *entity.Marker {
			m, err := immich.GetMarker(r.Context(), s.client, s.cfg.UpstreamURL.String(), immich.AuthHeaders(r.Header), assetID)
			if err != nil && r.Context().Err() == nil {
				s.logger.Warnf("App sync: could not read the marker of asset %s: %v", assetID, err)
			}
			return m
		},
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

// The app can also name a hidden entry in the body of a request. Two places
// in the app do, both by matching a photo on the phone to "its" server asset
// through the checksum, which finds the hidden entry:
//
//   - On iOS it records the photo's iCloud id on that asset, for all such
//     assets at once (PUT /api/assets/metadata). Immich checks every id of
//     the batch and refuses all of it over one it does not know.
//   - After a photo was edited on the phone and uploaded again, it stacks the
//     new upload on the earlier one (POST /api/stacks).
//
// So a hidden id in a JSON body is turned into the id of the real asset,
// which is what the app means. The bulk metadata request is the exception:
// there the real asset is usually in the same batch already, and one batch
// must not name an asset twice, so the hidden entries are taken out.

const (
	bulkMetadataPath = "/api/assets/metadata"
	// maxRewrittenBody bounds the request bodies that are read to look for
	// hidden ids. Bigger ones are passed on as they are.
	maxRewrittenBody = 8 << 20
)

// hiddenIDInJSON matches a hidden id as a JSON string. No real asset id has
// c, d, e or f as the first digit of its fourth group.
var hiddenIDInJSON = regexp.MustCompile(`"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[c-fC-F][0-9a-fA-F]{3}-[0-9a-fA-F]{12}"`)

// rewriteHiddenBody replaces hidden ids in a JSON request body. It reports
// whether it answered the request itself.
func (s *Server) rewriteHiddenBody(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}
	if r.Body == nil || r.ContentLength <= 0 || r.ContentLength > maxRewrittenBody ||
		!strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRewrittenBody+1))
	if err != nil {
		http.Error(w, "failed to read request", http.StatusBadRequest)
		return true
	}
	restore := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}
	if !hiddenIDInJSON.Match(body) {
		restore(body)
		return false
	}

	if r.Method == http.MethodPut && r.URL.Path == bulkMetadataPath {
		var req struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if json.Unmarshal(body, &req) != nil {
			restore(body)
			return false
		}
		kept := make([]map[string]json.RawMessage, 0, len(req.Items))
		for _, it := range req.Items {
			var id string
			_ = json.Unmarshal(it["assetId"], &id)
			if _, hidden := realID(id); !hidden {
				kept = append(kept, it)
			}
		}
		s.logger.Debugf("App sync: left %d hidden entr(ies) out of a metadata request", len(req.Items)-len(kept))
		if len(kept) == 0 {
			// Nothing left to tell Immich.
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = io.WriteString(w, "[]")
			return true
		}
		out, _ := json.Marshal(map[string]any{"items": kept})
		restore(out)
		return false
	}

	restore(hiddenIDInJSON.ReplaceAllFunc(body, func(m []byte) []byte {
		real, ok := realID(string(m[1 : len(m)-1]))
		if !ok {
			return m
		}
		return []byte(`"` + real + `"`)
	}))
	return false
}

// serveAppSync handles the requests the app sync feature cares about. It
// reports whether the request was answered.
func (s *Server) serveAppSync(w http.ResponseWriter, r *http.Request) bool {
	if p, changed := rewriteHiddenIDs(r.URL.Path); changed {
		r.URL.Path, r.URL.RawPath = p, ""
	}
	if s.rewriteHiddenBody(w, r) {
		return true
	}
	switch {
	case r.URL.Path == socketPath && isWebsocketUpgrade(r):
		return s.serveSocket(w, r)
	case r.Method == http.MethodPost && r.URL.Path == syncStreamPath:
		s.serveSyncStream(w, r)
		return true
	case r.Method == http.MethodPost && r.URL.Path == syncAckPath:
		return s.serveSyncAck(w, r)
	}
	return false
}
