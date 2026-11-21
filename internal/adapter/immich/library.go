package immich

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- content identity matching Immich, not security
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/httpclient"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// LibraryClient implements port.LibraryAPI on Immich's REST API with an API
// key.
type LibraryClient struct {
	baseURL string
	apiKey  string
	hc      *http.Client
	// PageSize is the number of assets per ListAssets page.
	PageSize int
}

var _ port.LibraryAPI = (*LibraryClient)(nil)

func NewLibraryClient(baseURL, apiKey string, hc *http.Client) *LibraryClient {
	if hc == nil {
		hc = httpclient.New(httpclient.Options{})
	}
	return &LibraryClient{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, hc: hc, PageSize: 250}
}

// apiError is a non-2xx answer from Immich.
type apiError struct {
	method, path string
	status       int
	body         string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("immich %s %s: status %d: %s", e.method, e.path, e.status, e.body)
}

// call sends a JSON request and decodes a JSON answer into out (when not nil).
func (c *LibraryClient) call(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("immich %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &apiError{method: method, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type assetDTO struct {
	ID               string    `json:"id"`
	OwnerID          string    `json:"ownerId"`
	LibraryID        *string   `json:"libraryId"`
	Type             string    `json:"type"`
	OriginalFileName string    `json:"originalFileName"`
	Checksum         string    `json:"checksum"`
	FileCreatedAt    time.Time `json:"fileCreatedAt"`
	FileModifiedAt   time.Time `json:"fileModifiedAt"`
	LocalDateTime    time.Time `json:"localDateTime"`
	IsFavorite       bool      `json:"isFavorite"`
	IsTrashed        bool      `json:"isTrashed"`
	IsEdited         bool      `json:"isEdited"`
	IsOffline        bool      `json:"isOffline"`
	Visibility       string    `json:"visibility"`
	// A string in older servers, milliseconds as a number in newer ones.
	Duration         json.RawMessage `json:"duration"`
	LivePhotoVideoID *string         `json:"livePhotoVideoId"`
	Thumbhash        *string         `json:"thumbhash"`
	ExifInfo         *struct {
		FileSizeInByte   *int64   `json:"fileSizeInByte"`
		DateTimeOriginal *string  `json:"dateTimeOriginal"`
		TimeZone         *string  `json:"timeZone"`
		Latitude         *float64 `json:"latitude"`
		Longitude        *float64 `json:"longitude"`
		Description      *string  `json:"description"`
		Rating           *float64 `json:"rating"`
		ExifImageWidth   *float64 `json:"exifImageWidth"`
		ExifImageHeight  *float64 `json:"exifImageHeight"`
		Make             *string  `json:"make"`
		Model            *string  `json:"model"`
		LensModel        *string  `json:"lensModel"`
		Orientation      *string  `json:"orientation"`
		ProjectionType   *string  `json:"projectionType"`
	} `json:"exifInfo"`
	Tags []struct {
		ID string `json:"id"`
	} `json:"tags"`
	People []struct {
		ID string `json:"id"`
	} `json:"people"`
	Stack *struct {
		ID             string `json:"id"`
		PrimaryAssetID string `json:"primaryAssetId"`
	} `json:"stack"`
}

// rawScalar renders a JSON string or number as text; null is "".
func rawScalar(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return s
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (d *assetDTO) toEntity() (entity.LibraryAsset, error) {
	a := entity.LibraryAsset{
		ID:               d.ID,
		OwnerID:          d.OwnerID,
		LibraryID:        str(d.LibraryID),
		Type:             d.Type,
		OriginalFileName: d.OriginalFileName,
		FileCreatedAt:    d.FileCreatedAt,
		FileModifiedAt:   d.FileModifiedAt,
		LocalDateTime:    d.LocalDateTime,
		IsFavorite:       d.IsFavorite,
		IsTrashed:        d.IsTrashed,
		IsEdited:         d.IsEdited,
		IsOffline:        d.IsOffline,
		Visibility:       d.Visibility,
		Duration:         rawScalar(d.Duration),
		LivePhotoVideoID: str(d.LivePhotoVideoID),
	}
	sum, err := base64.StdEncoding.DecodeString(d.Checksum)
	if err != nil || len(sum) != sha1.Size {
		return a, fmt.Errorf("asset %s: unexpected checksum %q", d.ID, d.Checksum)
	}
	a.Checksum = hex.EncodeToString(sum)
	if e := d.ExifInfo; e != nil {
		if e.FileSizeInByte != nil {
			a.Size = *e.FileSizeInByte
		}
		a.DateTimeOriginal = str(e.DateTimeOriginal)
		a.TimeZone = str(e.TimeZone)
		a.Latitude, a.Longitude = e.Latitude, e.Longitude
		a.Description = str(e.Description)
		a.Rating = e.Rating
		if e.ExifImageWidth != nil {
			a.Width = int(*e.ExifImageWidth)
		}
		if e.ExifImageHeight != nil {
			a.Height = int(*e.ExifImageHeight)
		}
		a.Make, a.Model, a.LensModel = str(e.Make), str(e.Model), str(e.LensModel)
		a.Orientation, a.ProjectionType = str(e.Orientation), str(e.ProjectionType)
	}
	// Immich has read the file once the thumbnail exists and, for images,
	// the dimensions are known.
	a.Ready = d.Thumbhash != nil && (a.Width > 0 || d.Type != "IMAGE")
	for _, t := range d.Tags {
		a.TagIDs = append(a.TagIDs, t.ID)
	}
	for _, p := range d.People {
		a.PersonIDs = append(a.PersonIDs, p.ID)
	}
	if d.Stack != nil {
		a.StackID, a.StackPrimaryID = d.Stack.ID, d.Stack.PrimaryAssetID
	}
	return a, nil
}

func (c *LibraryClient) CurrentUserID(ctx context.Context) (string, error) {
	var me struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodGet, "/users/me", nil, &me); err != nil {
		return "", err
	}
	if me.ID == "" {
		return "", fmt.Errorf("immich did not report the API key's user")
	}
	return me.ID, nil
}

func (c *LibraryClient) ListAssets(ctx context.Context, page int) ([]entity.LibraryAsset, int, error) {
	var out struct {
		Assets struct {
			Items    []assetDTO `json:"items"`
			NextPage *string    `json:"nextPage"`
		} `json:"assets"`
	}
	body := map[string]any{"page": page, "size": c.PageSize, "withExif": true, "order": "asc"}
	if err := c.call(ctx, http.MethodPost, "/search/metadata", body, &out); err != nil {
		return nil, 0, err
	}
	assets := make([]entity.LibraryAsset, 0, len(out.Assets.Items))
	for i := range out.Assets.Items {
		a, err := out.Assets.Items[i].toEntity()
		if err != nil {
			return nil, 0, err
		}
		assets = append(assets, a)
	}
	next := 0
	if out.Assets.NextPage != nil {
		next, _ = strconv.Atoi(*out.Assets.NextPage)
	}
	return assets, next, nil
}

// MatchingAssetIDs runs Immich's asset search with the given filter and
// returns the ids of everything it finds.
func (c *LibraryClient) MatchingAssetIDs(ctx context.Context, filter json.RawMessage) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		var out struct {
			Assets struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
				NextCursor *string `json:"nextCursor"`
			} `json:"assets"`
		}
		body := map[string]any{"filter": filter, "size": c.PageSize,
			"orderBy": map[string]string{"field": "fileCreatedAt", "direction": "asc"}}
		if cursor != "" {
			body["cursor"] = cursor
		}
		if err := c.call(ctx, http.MethodPost, "/search/metadata", body, &out); err != nil {
			return nil, err
		}
		for _, it := range out.Assets.Items {
			ids = append(ids, it.ID)
		}
		if out.Assets.NextCursor == nil || *out.Assets.NextCursor == "" || len(out.Assets.Items) == 0 {
			return ids, nil
		}
		cursor = *out.Assets.NextCursor
	}
}

func (c *LibraryClient) GetAsset(ctx context.Context, id string) (*entity.LibraryAsset, error) {
	var d assetDTO
	if err := c.call(ctx, http.MethodGet, "/assets/"+url.PathEscape(id), nil, &d); err != nil {
		// Immich answers 400 for an asset that does not exist or is not
		// readable with this key.
		var ae *apiError
		if errors.As(err, &ae) && (ae.status == http.StatusBadRequest || ae.status == http.StatusNotFound) {
			return nil, fmt.Errorf("%w: %s", entity.ErrAssetNotFound, id)
		}
		return nil, err
	}
	a, err := d.toEntity()
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (c *LibraryClient) GetMetadata(ctx context.Context, id string) ([]entity.MetadataItem, error) {
	var items []entity.MetadataItem
	if err := c.call(ctx, http.MethodGet, "/assets/"+url.PathEscape(id)+"/metadata", nil, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (c *LibraryClient) PutMetadata(ctx context.Context, id string, items []entity.MetadataItem) error {
	return c.call(ctx, http.MethodPut, "/assets/"+url.PathEscape(id)+"/metadata", map[string]any{"items": items}, nil)
}

func (c *LibraryClient) DownloadOriginal(ctx context.Context, id, dst string) (string, error) {
	path := "/assets/" + url.PathEscape(id) + "/original"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("x-api-key", c.apiKey)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("immich GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", &apiError{method: http.MethodGet, path: path, status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha1.New() // #nosec G401 -- content identity matching Immich, not security
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("downloading asset %s: %w", id, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *LibraryClient) Upload(ctx context.Context, u entity.LibraryUpload) (*entity.LibraryUploadResult, error) {
	metadata := ""
	if len(u.Metadata) > 0 {
		b, err := json.Marshal(u.Metadata)
		if err != nil {
			return nil, err
		}
		metadata = string(b)
	}
	const layout = "2006-01-02T15:04:05.000Z"
	fields := [][2]string{
		// Older servers require the two device fields; current ones ignore them.
		{"deviceAssetId", fmt.Sprintf("%s-%d", u.Filename, u.FileCreatedAt.Unix())},
		{"deviceId", "immich-optimizer"},
		{"fileCreatedAt", u.FileCreatedAt.UTC().Format(layout)},
		{"fileModifiedAt", u.FileModifiedAt.UTC().Format(layout)},
		{"isFavorite", strconv.FormatBool(u.IsFavorite)},
	}
	if u.Visibility != "" {
		fields = append(fields, [2]string{"visibility", u.Visibility})
	}
	if metadata != "" {
		fields = append(fields, [2]string{"metadata", metadata})
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for _, f := range fields {
				if err := mw.WriteField(f[0], f[1]); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile("assetData", u.Filename)
			if err != nil {
				return err
			}
			file, err := os.Open(u.Path)
			if err != nil {
				return err
			}
			defer file.Close()
			if _, err := io.Copy(part, file); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/assets", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("immich POST /assets: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, &apiError{method: http.MethodPost, path: "/assets", status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	var r uploadResponse
	if err := json.Unmarshal(body, &r); err != nil || r.ID == "" {
		return nil, fmt.Errorf("immich POST /assets: unexpected answer %q", string(body))
	}
	return &entity.LibraryUploadResult{ID: r.ID, Duplicate: r.Status == "duplicate"}, nil
}

func (c *LibraryClient) CopyAsset(ctx context.Context, sourceID, targetID string) error {
	return c.call(ctx, http.MethodPut, "/assets/copy", map[string]any{"sourceId": sourceID, "targetId": targetID}, nil)
}

func (c *LibraryClient) UpdateAsset(ctx context.Context, id string, u entity.LibraryAssetUpdate) error {
	body := map[string]any{}
	if u.Description != nil {
		body["description"] = *u.Description
	}
	if u.Rating != nil {
		body["rating"] = *u.Rating
	}
	if u.DateTimeOriginal != nil {
		body["dateTimeOriginal"] = *u.DateTimeOriginal
	}
	if u.Latitude != nil && u.Longitude != nil {
		body["latitude"], body["longitude"] = *u.Latitude, *u.Longitude
	}
	if u.IsFavorite != nil {
		body["isFavorite"] = *u.IsFavorite
	}
	if u.Visibility != nil {
		body["visibility"] = *u.Visibility
	}
	if u.LivePhotoVideoID != nil {
		body["livePhotoVideoId"] = *u.LivePhotoVideoID
	}
	if len(body) == 0 {
		return nil
	}
	return c.call(ctx, http.MethodPut, "/assets/"+url.PathEscape(id), body, nil)
}

func (c *LibraryClient) Albums(ctx context.Context, assetID string) ([]entity.LibraryAlbum, error) {
	var out []struct {
		ID         string  `json:"id"`
		AlbumName  string  `json:"albumName"`
		Shared     bool    `json:"shared"`
		AlbumUsers []any   `json:"albumUsers"`
		OwnerID    string  `json:"ownerId"`
		Cover      *string `json:"albumThumbnailAssetId"`
	}
	if err := c.call(ctx, http.MethodGet, "/albums?assetId="+url.QueryEscape(assetID), nil, &out); err != nil {
		return nil, err
	}
	albums := make([]entity.LibraryAlbum, 0, len(out))
	for _, a := range out {
		// More than one album user means somebody else can see and react to it.
		albums = append(albums, entity.LibraryAlbum{ID: a.ID, Name: a.AlbumName, Shared: a.Shared || len(a.AlbumUsers) > 1,
			OwnerID: a.OwnerID, CoverAssetID: str(a.Cover)})
	}
	return albums, nil
}

func (c *LibraryClient) SetAlbumCover(ctx context.Context, albumID, assetID string) error {
	return c.call(ctx, http.MethodPatch, "/albums/"+url.PathEscape(albumID), map[string]any{"albumThumbnailAssetId": assetID}, nil)
}

func (c *LibraryClient) ActivityCount(ctx context.Context, albumID, assetID string) (int, error) {
	var out struct {
		Comments int `json:"comments"`
		Likes    int `json:"likes"`
	}
	q := "/activities/statistics?albumId=" + url.QueryEscape(albumID) + "&assetId=" + url.QueryEscape(assetID)
	if err := c.call(ctx, http.MethodGet, q, nil, &out); err != nil {
		return 0, err
	}
	return out.Comments + out.Likes, nil
}

func (c *LibraryClient) TagAsset(ctx context.Context, assetID string, tagIDs []string) error {
	if len(tagIDs) == 0 {
		return nil
	}
	return c.call(ctx, http.MethodPut, "/tags/assets", map[string]any{"assetIds": []string{assetID}, "tagIds": tagIDs}, nil)
}

func (c *LibraryClient) Faces(ctx context.Context, assetID string) ([]entity.LibraryFace, error) {
	var out []struct {
		X1          int `json:"boundingBoxX1"`
		Y1          int `json:"boundingBoxY1"`
		X2          int `json:"boundingBoxX2"`
		Y2          int `json:"boundingBoxY2"`
		ImageWidth  int `json:"imageWidth"`
		ImageHeight int `json:"imageHeight"`
		Person      *struct {
			ID string `json:"id"`
		} `json:"person"`
	}
	if err := c.call(ctx, http.MethodGet, "/faces?id="+url.QueryEscape(assetID), nil, &out); err != nil {
		return nil, err
	}
	var faces []entity.LibraryFace
	for _, f := range out {
		if f.Person == nil {
			continue // unassigned faces are regenerated by Immich's detection
		}
		faces = append(faces, entity.LibraryFace{PersonID: f.Person.ID, X1: f.X1, Y1: f.Y1, X2: f.X2, Y2: f.Y2, ImageWidth: f.ImageWidth, ImageHeight: f.ImageHeight})
	}
	return faces, nil
}

func (c *LibraryClient) CreateFace(ctx context.Context, assetID string, f entity.LibraryFace) error {
	return c.call(ctx, http.MethodPost, "/faces", map[string]any{
		"assetId": assetID, "personId": f.PersonID,
		"x": f.X1, "y": f.Y1, "width": f.X2 - f.X1, "height": f.Y2 - f.Y1,
		"imageWidth": f.ImageWidth, "imageHeight": f.ImageHeight,
	}, nil)
}

func (c *LibraryClient) SharedLinkIDs(ctx context.Context, assetID string) ([]string, error) {
	var list []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	}
	if err := c.call(ctx, http.MethodGet, "/shared-links", nil, &list); err != nil {
		return nil, err
	}
	var ids []string
	for _, l := range list {
		if l.Type != "INDIVIDUAL" {
			continue // album links follow album membership
		}
		// The list only shows part of each link's assets; the link itself
		// shows them all. Read fresh every time: links can change mid-run.
		var link struct {
			Assets []struct {
				ID string `json:"id"`
			} `json:"assets"`
		}
		if err := c.call(ctx, http.MethodGet, "/shared-links/"+url.PathEscape(l.ID), nil, &link); err != nil {
			return nil, err
		}
		for _, a := range link.Assets {
			if a.ID == assetID {
				ids = append(ids, l.ID)
				break
			}
		}
	}
	return ids, nil
}

// MemoryIDs lists the owner's memories and returns which assets are in which.
func (c *LibraryClient) MemoryIDs(ctx context.Context) (map[string][]string, error) {
	const size = 100
	out := map[string][]string{}
	seen := map[string]bool{}
	for page := 1; ; page++ {
		var list []struct {
			ID     string `json:"id"`
			Assets []struct {
				ID string `json:"id"`
			} `json:"assets"`
		}
		if err := c.call(ctx, http.MethodGet, fmt.Sprintf("/memories?size=%d&page=%d", size, page), nil, &list); err != nil {
			return nil, err
		}
		fresh := 0
		for _, m := range list {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			fresh++
			for _, a := range m.Assets {
				out[a.ID] = append(out[a.ID], m.ID)
			}
		}
		// The last page, or a server that ignores the page number.
		if len(list) < size || fresh == 0 {
			return out, nil
		}
	}
}

// AddToMemory adds an asset to a memory. Immich answers 200 with a result per
// asset; anything but success or "already there" is an error.
func (c *LibraryClient) AddToMemory(ctx context.Context, memoryID, assetID string) error {
	var results []struct {
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := c.call(ctx, http.MethodPut, "/memories/"+url.PathEscape(memoryID)+"/assets", map[string]any{"ids": []string{assetID}}, &results); err != nil {
		return err
	}
	for _, r := range results {
		if !r.Success && r.Error != "duplicate" {
			return fmt.Errorf("immich refused to add asset %s to memory %s: %s", assetID, memoryID, r.Error)
		}
	}
	return nil
}

func (c *LibraryClient) AddToSharedLink(ctx context.Context, linkID, assetID string) error {
	return c.call(ctx, http.MethodPut, "/shared-links/"+url.PathEscape(linkID)+"/assets", map[string]any{"assetIds": []string{assetID}}, nil)
}

func (c *LibraryClient) SetStackPrimary(ctx context.Context, stackID, assetID string) error {
	return c.call(ctx, http.MethodPut, "/stacks/"+url.PathEscape(stackID), map[string]any{"primaryAssetId": assetID}, nil)
}

func (c *LibraryClient) Edits(ctx context.Context, assetID string) ([]entity.LibraryEdit, error) {
	var out struct {
		Edits []entity.LibraryEdit `json:"edits"`
	}
	if err := c.call(ctx, http.MethodGet, "/assets/"+url.PathEscape(assetID)+"/edits", nil, &out); err != nil {
		return nil, err
	}
	return out.Edits, nil
}

func (c *LibraryClient) SetEdits(ctx context.Context, assetID string, edits []entity.LibraryEdit) error {
	return c.call(ctx, http.MethodPut, "/assets/"+url.PathEscape(assetID)+"/edits", map[string]any{"edits": edits}, nil)
}

func (c *LibraryClient) Trash(ctx context.Context, ids []string) error {
	return c.call(ctx, http.MethodDelete, "/assets", map[string]any{"ids": ids, "force": false}, nil)
}

func (c *LibraryClient) Delete(ctx context.Context, ids []string) error {
	return c.call(ctx, http.MethodDelete, "/assets", map[string]any{"ids": ids, "force": true}, nil)
}
