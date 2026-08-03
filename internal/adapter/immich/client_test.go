package immich

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

func TestUploadAssetWithFilenameSuccess(t *testing.T) {
	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "test.jpg")
	fileContent := []byte("fake image data")
	if err := os.WriteFile(testFilePath, fileContent, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	var (
		receivedAPIKey   string
		receivedFilename string
		receivedContent  []byte
		receivedDeviceID string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/assets" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		receivedAPIKey = r.Header.Get("x-api-key")

		err := r.ParseMultipartForm(10 << 20)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		receivedDeviceID = r.FormValue("deviceId")

		file, header, err := r.FormFile("assetData")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()

		receivedFilename = header.Filename
		receivedContent, _ = io.ReadAll(file)

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id": "asset-123"}`))
	}))
	defer server.Close()

	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	c := NewClient(server.URL, "test-api-key", 10, logger)

	err := c.UploadAssetWithFilename(context.Background(), testFilePath, "custom_name.jpg")
	if err != nil {
		t.Fatalf("UploadAssetWithFilename failed: %v", err)
	}

	if receivedAPIKey != "test-api-key" {
		t.Errorf("expected API key 'test-api-key', got '%s'", receivedAPIKey)
	}
	if receivedFilename != "custom_name.jpg" {
		t.Errorf("expected filename 'custom_name.jpg', got '%s'", receivedFilename)
	}
	if string(receivedContent) != string(fileContent) {
		t.Errorf("expected content '%s', got '%s'", string(fileContent), string(receivedContent))
	}
	if receivedDeviceID != "immich-optimizer" {
		t.Errorf("expected deviceId 'immich-optimizer', got '%s'", receivedDeviceID)
	}
}

func TestUploadAssetErrorStatus(t *testing.T) {
	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "test.jpg")
	os.WriteFile(testFilePath, []byte("data"), 0644)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	c := NewClient(server.URL, "bad-key", 10, logger)

	err := c.UploadAsset(context.Background(), testFilePath)
	if err == nil {
		t.Fatalf("expected error on 401 unauthorized, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected error message to contain '401', got: %v", err)
	}
}

func TestUploadAssetRetryOnServerError(t *testing.T) {
	tempDir := t.TempDir()
	testFilePath := filepath.Join(tempDir, "test.jpg")
	os.WriteFile(testFilePath, []byte("retry content"), 0644)

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attempts, 1)
		if count < 2 {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id": "ok"}`))
	}))
	defer server.Close()

	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	c := NewClient(server.URL, "key", 10, logger)

	err := c.UploadAsset(context.Background(), testFilePath)
	if err != nil {
		t.Fatalf("expected upload to succeed after retry, got: %v", err)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("expected 2 attempts (1 failure + 1 retry success), got %d", attempts)
	}
}
