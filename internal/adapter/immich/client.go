package immich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/httpclient"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type client struct {
	BaseURL    string
	APIKey     string
	logger     *logger.Logger
	httpClient *http.Client
}

// NewClient returns an uploader. httpClient should come from
// httpclient.New (transport-level timeouts, no whole-request timeout);
// nil uses httpclient defaults.
func NewClient(baseURL, apiKey string, httpClient *http.Client, log *logger.Logger) port.AssetUploader {
	if httpClient == nil {
		httpClient = httpclient.New(httpclient.Options{})
	}
	return &client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		logger:     log,
		httpClient: httpClient,
	}
}

type statusError struct {
	statusCode int
	body       string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("upload failed with status %d: %s", e.statusCode, e.body)
}

func (c *client) UploadAsset(ctx context.Context, u entity.Upload) (entity.UploadResult, error) {
	var res entity.UploadResult
	filePath, filename := u.Path, u.Filename
	stat, err := os.Stat(filePath)
	if err != nil {
		return res, fmt.Errorf("unable to get file info: %w", err)
	}
	if u.ModTime.IsZero() {
		u.ModTime = stat.ModTime()
	}

	maxRetries := 3
	backoff := 1 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		res, err = c.doUpload(ctx, u, stat)
		if err == nil {
			return res, nil
		}

		if ctx.Err() != nil {
			return res, err
		}

		var isTransient bool
		var stErr *statusError
		if errors.As(err, &stErr) {
			if stErr.statusCode >= 500 {
				isTransient = true
			}
		} else {
			isTransient = true
		}

		if !isTransient || attempt == maxRetries {
			return res, err
		}

		c.logger.Printf("Upload attempt %d/%d failed for %s (%v), retrying in %v...", attempt, maxRetries, filename, err, backoff)

		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
	}

	return res, err
}

func (c *client) doUpload(ctx context.Context, u entity.Upload, stat os.FileInfo) (entity.UploadResult, error) {
	var res entity.UploadResult
	filePath, filename := u.Path, u.Filename
	metadata := ""
	if u.Marker != nil {
		var err error
		if metadata, err = u.Marker.MergeIntoMetadata(""); err != nil {
			return res, fmt.Errorf("unable to encode asset metadata: %w", err)
		}
	}

	pr, pw := io.Pipe()
	defer pr.Close()
	writer := multipart.NewWriter(pw)

	go func() {
		var copyErr error
		defer func() {
			if copyErr != nil && !errors.Is(copyErr, io.ErrClosedPipe) {
				c.logger.Printf("upload multipart writer error: %v", copyErr)
			}
			pw.CloseWithError(copyErr)
		}()

		file, err := os.Open(filePath)
		if err != nil {
			copyErr = fmt.Errorf("unable to open file: %w", err)
			return
		}
		defer file.Close()

		// Add required fields
		deviceAssetId := fmt.Sprintf("%s-%d", filename, u.ModTime.Unix())
		deviceId := "immich-optimizer"

		// The dates of the original file, in UTC as the "Z" suffix says.
		fileCreatedAt := u.ModTime.UTC().Format("2006-01-02T15:04:05.000Z")
		fileModifiedAt := fileCreatedAt

		if copyErr = writer.WriteField("deviceAssetId", deviceAssetId); copyErr != nil {
			return
		}
		if copyErr = writer.WriteField("deviceId", deviceId); copyErr != nil {
			return
		}
		if copyErr = writer.WriteField("fileCreatedAt", fileCreatedAt); copyErr != nil {
			return
		}
		if copyErr = writer.WriteField("fileModifiedAt", fileModifiedAt); copyErr != nil {
			return
		}

		if metadata != "" {
			if copyErr = writer.WriteField("metadata", metadata); copyErr != nil {
				return
			}
		}

		var part io.Writer
		part, copyErr = writer.CreateFormFile("assetData", filename)
		if copyErr != nil {
			return
		}

		if _, copyErr = io.Copy(part, file); copyErr != nil {
			return
		}

		copyErr = writer.Close()
	}()

	url := fmt.Sprintf("%s/api/assets", c.BaseURL)
	req, err := http.NewRequestWithContext(ctx, "POST", url, pr)
	if err != nil {
		pr.Close()
		return res, fmt.Errorf("unable to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-api-key", c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("unable to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return res, &statusError{
			statusCode: resp.StatusCode,
			body:       string(body),
		}
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var answer uploadResponse
	_ = json.Unmarshal(body, &answer)
	res.AssetID = answer.ID
	if id := DuplicateAssetID(body); id != "" {
		res.Duplicate = true
		c.logger.Printf("Immich already has %s (%s) as asset %s", filename, utils.HumanReadableSize(stat.Size()), id)
		if u.Marker != nil {
			auth := http.Header{"X-Api-Key": []string{c.APIKey}}
			if err := EnsureMarker(ctx, c.httpClient, c.BaseURL, auth, id, u.Marker); err != nil {
				// The file is safely in Immich; a missing marker is not a failed upload.
				c.logger.Warnf("Could not record the optimizer marker on existing asset %s: %v", id, err)
			}
		}
		return res, nil
	}

	c.logger.Printf("Successfully uploaded %s (%s)", filename, utils.HumanReadableSize(stat.Size()))
	return res, nil
}
