package immich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type client struct {
	BaseURL        string
	APIKey         string
	TimeoutSeconds int
	logger         *logger.Logger
	httpClient     *http.Client
}

func NewClient(baseURL, apiKey string, timeoutSeconds int, log *logger.Logger) port.AssetUploader {
	return &client{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		APIKey:         apiKey,
		TimeoutSeconds: timeoutSeconds,
		logger:         log,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

type statusError struct {
	statusCode int
	body       string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("upload failed with status %d: %s", e.statusCode, e.body)
}

func (c *client) UploadAsset(ctx context.Context, filePath string) error {
	return c.UploadAssetWithFilename(ctx, filePath, filepath.Base(filePath))
}

func (c *client) UploadAssetWithFilename(ctx context.Context, filePath, filename string) error {
	stat, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("unable to get file info: %w", err)
	}

	maxRetries := 3
	backoff := 1 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		err = c.doUpload(ctx, filePath, filename, stat)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			return err
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
			return err
		}

		c.logger.Printf("Upload attempt %d/%d failed for %s (%v), retrying in %v...", attempt, maxRetries, filename, err, backoff)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		backoff *= 2
	}

	return err
}

func (c *client) doUpload(ctx context.Context, filePath, filename string, stat os.FileInfo) error {
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
		deviceAssetId := fmt.Sprintf("%s-%d", filename, stat.ModTime().Unix())
		deviceId := "immich-optimizer"

		// Convert times to RFC3339 format
		fileCreatedAt := stat.ModTime().Format("2006-01-02T15:04:05.000Z")
		fileModifiedAt := stat.ModTime().Format("2006-01-02T15:04:05.000Z")

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
		return fmt.Errorf("unable to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-api-key", c.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unable to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &statusError{
			statusCode: resp.StatusCode,
			body:       string(body),
		}
	}

	c.logger.Printf("Successfully uploaded %s (%s)", filename, utils.HumanReadableSize(stat.Size()))
	return nil
}
