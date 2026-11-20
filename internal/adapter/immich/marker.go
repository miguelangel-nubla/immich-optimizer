package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// uploadResponse is Immich's answer to POST /api/assets.
type uploadResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// DuplicateAssetID returns the id of the existing asset when body is Immich's
// "duplicate" answer to an upload, and "" otherwise.
func DuplicateAssetID(body []byte) string {
	var r uploadResponse
	if err := json.Unmarshal(body, &r); err != nil || r.Status != "duplicate" {
		return ""
	}
	return r.ID
}

// EnsureMarker writes marker to an existing asset unless that asset already
// has one. Immich ignores the upload form's metadata when it answers
// "duplicate", so this is how an already stored conversion gets its marker.
// auth carries the caller's credentials (API key, or the proxied client's
// own headers); it is sent as it is and never logged.
//
// With refresh, a marker that is already there is written again unchanged.
// Immich then sends it to its sync clients once more, which is how a phone
// that missed it gets it.
func EnsureMarker(ctx context.Context, hc *http.Client, baseURL string, auth http.Header, assetID string, marker *entity.Marker, refresh bool) error {
	base := strings.TrimRight(baseURL, "/") + "/api/assets/" + url.PathEscape(assetID) + "/metadata"

	do := func(method, target string, body []byte) (*http.Response, error) {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, r)
		if err != nil {
			return nil, err
		}
		for k, vv := range auth {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return hc.Do(req)
	}

	// Immich answers 200 when the key exists (and 400 when it does not).
	resp, err := do(http.MethodGet, base+"/"+url.PathEscape(entity.MarkerKey), nil)
	if err != nil {
		return err
	}
	existing, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var value json.RawMessage
	if resp.StatusCode == http.StatusOK {
		if !refresh {
			return nil
		}
		var item struct {
			Value json.RawMessage `json:"value"`
		}
		if json.Unmarshal(existing, &item) != nil || len(item.Value) == 0 {
			return nil
		}
		value = item.Value
	} else if value, err = json.Marshal(marker); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"items": []map[string]any{{"key": entity.MarkerKey, "value": json.RawMessage(value)}},
	})
	if err != nil {
		return err
	}
	resp, err = do(http.MethodPut, base, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("writing asset metadata: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// GetMarker reads the marker of an asset with the caller's credentials. It
// returns nil when the asset has none.
func GetMarker(ctx context.Context, hc *http.Client, baseURL string, auth http.Header, assetID string) (*entity.Marker, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/assets/" + url.PathEscape(assetID) + "/metadata/" + url.PathEscape(entity.MarkerKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for k, vv := range auth {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// Immich answers 400 when the asset has no such key.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading asset metadata: status %d", resp.StatusCode)
	}
	var item struct {
		Value *entity.Marker `json:"value"`
	}
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, err
	}
	return item.Value, nil
}

// AuthHeaders returns the credential headers of h, for follow-up requests on
// behalf of the same caller.
func AuthHeaders(h http.Header) http.Header {
	out := http.Header{}
	for _, k := range []string{"Authorization", "X-Api-Key", "Cookie", "X-Immich-User-Token", "X-Immich-Session-Token", "X-Immich-Share-Key"} {
		if vv := h.Values(k); len(vv) > 0 {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), vv...)
		}
	}
	return out
}
