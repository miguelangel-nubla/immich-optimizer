package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type ImmichClient struct {
	BaseURL        string
	APIKey         string
	TimeoutSeconds int
	logger         *customLogger
}

func NewImmichClient(baseURL, apiKey string, timeoutSeconds int, logger *customLogger) *ImmichClient {
	return &ImmichClient{
		BaseURL:        baseURL,
		APIKey:         apiKey,
		TimeoutSeconds: timeoutSeconds,
		logger:         logger,
	}
}

func (c *ImmichClient) UploadAsset(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("unable to open file: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("unable to get file info: %w", err)
	}

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	// Add required fields
	filename := filepath.Base(filePath)
	deviceAssetId := fmt.Sprintf("%s-%d", filename, stat.ModTime().Unix())
	deviceId := "immich-optimizer"

	// Convert times to RFC3339 format
	fileCreatedAt := stat.ModTime().Format("2006-01-02T15:04:05.000Z")
	fileModifiedAt := stat.ModTime().Format("2006-01-02T15:04:05.000Z")

	writer.WriteField("deviceAssetId", deviceAssetId)
	writer.WriteField("deviceId", deviceId)
	writer.WriteField("fileCreatedAt", fileCreatedAt)
	writer.WriteField("fileModifiedAt", fileModifiedAt)

	part, err := writer.CreateFormFile("assetData", filename)
	if err != nil {
		return fmt.Errorf("unable to create form file: %w", err)
	}

	_, err = io.Copy(part, file)
	if err != nil {
		return fmt.Errorf("unable to copy file to form: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return fmt.Errorf("unable to close multipart writer: %w", err)
	}

	url := fmt.Sprintf("%s/api/assets", c.BaseURL)
	req, err := http.NewRequest("POST", url, &buffer)
	if err != nil {
		return fmt.Errorf("unable to create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-api-key", c.APIKey)

	client := &http.Client{
		Timeout: time.Duration(c.TimeoutSeconds) * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unable to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	c.logger.Printf("Successfully uploaded %s (%s)", filename, humanReadableSize(stat.Size()))
	return nil
}
