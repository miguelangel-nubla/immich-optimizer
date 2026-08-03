package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

type mockProcessor struct {
	processFunc func(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error)
}

func (m *mockProcessor) Process(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error) {
	if m.processFunc != nil {
		return m.processFunc(ctx, filePath, tasks)
	}
	return nil, fmt.Errorf("mock processor unhandled")
}

func TestProxyPassthroughNonUpload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/server-info" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"res":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	logger := customlogger.NewWithLevel(nil, "error", "")

	server := NewServer(u, "127.0.0.1:0", "/api/assets", "assetData", &mockProcessor{}, nil, logger, 5, 10)

	req := httptest.NewRequest(http.MethodGet, "/api/server-info", nil)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"res":"ok"}` {
		t.Fatalf("expected response body `{\"res\":\"ok\"}`, got %s", rec.Body.String())
	}
}

func TestProxyUploadOptimization(t *testing.T) {
	var upstreamReceivedBody []byte
	var upstreamContentType string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamContentType = r.Header.Get("Content-Type")
		var err error
		upstreamReceivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream failed to read body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"asset-123"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	logger := customlogger.NewWithLevel(nil, "error", "")

	mockProc := &mockProcessor{
		processFunc: func(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error) {
			// Simulate optimization: create temporary optimized file smaller than original
			optFile, err := os.CreateTemp("", "opt-*.jpg")
			if err != nil {
				return nil, err
			}
			_, _ = optFile.Write([]byte("small"))
			optFile.Close()

			stat, _ := os.Stat(filePath)
			origSize := int64(100)
			if stat != nil {
				origSize = stat.Size()
			}

			return &entity.ProcessResult{
				ProcessedFilePath: optFile.Name(),
				ProcessedFilename: "test_opt.jpg",
				OriginalSize:      origSize,
				ProcessedSize:     5,
				Cleanup: func() {
					os.Remove(optFile.Name())
				},
			}, nil
		},
	}

	tasks := []entity.Task{
		{
			Name:       "jpeg-opt",
			Extensions: []string{"jpg", "jpeg"},
		},
	}

	server := NewServer(u, "127.0.0.1:0", "/api/assets", "assetData", mockProc, tasks, logger, 5, 10)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("assetData", "original.jpg")
	_, _ = fw.Write(bytes.Repeat([]byte("A"), 100))
	_ = mw.WriteField("deviceId", "phone-1")
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/assets", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(upstreamContentType, "multipart/form-data") {
		t.Fatalf("expected upstream content-type multipart/form-data, got %s", upstreamContentType)
	}

	// Verify upstream received optimized file "small" instead of 100 "A"s
	if !strings.Contains(string(upstreamReceivedBody), "small") {
		t.Fatalf("expected upstream body to contain optimized content 'small', got: %s", string(upstreamReceivedBody))
	}
	if strings.Contains(string(upstreamReceivedBody), strings.Repeat("A", 100)) {
		t.Fatalf("expected original unoptimized body to be replaced")
	}
}

func TestProxyUploadNonMatchingExtensionPassThrough(t *testing.T) {
	var upstreamReceivedBody []byte

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamReceivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("upstream failed to read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	logger := customlogger.NewWithLevel(nil, "error", "")

	mockProc := &mockProcessor{
		processFunc: func(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error) {
			t.Fatalf("processor should not be called for non-matching extension")
			return nil, nil
		},
	}

	tasks := []entity.Task{
		{
			Name:       "jpeg-opt",
			Extensions: []string{"jpg"},
		},
	}

	server := NewServer(u, "127.0.0.1:0", "/api/assets", "assetData", mockProc, tasks, logger, 5, 10)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("assetData", "document.pdf")
	_, _ = fw.Write([]byte("pdf-content"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/assets", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if !strings.Contains(string(upstreamReceivedBody), "pdf-content") {
		t.Fatalf("expected un-modified pdf content in body, got: %s", string(upstreamReceivedBody))
	}
}

func TestProxyAsyncJobRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"async":"ok"}`))
	}))
	defer upstream.Close()

	u, _ := url.Parse(upstream.URL)
	logger := customlogger.NewWithLevel(nil, "error", "")

	mockProc := &mockProcessor{
		processFunc: func(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error) {
			return &entity.ProcessResult{
				ProcessedFilePath: filePath,
				OriginalSize:      10,
				ProcessedSize:     10,
			}, nil
		},
	}

	server := NewServer(u, "127.0.0.1:0", "/api/assets", "assetData", mockProc, nil, logger, 5, 10)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("assetData", "image.jpg")
	_, _ = fw.Write([]byte("image-data"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/assets", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected status 307 Temporary Redirect for browser client, got %d", rec.Code)
	}

	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/_immich-upload-optimizer/wait?job=") {
		t.Fatalf("expected redirect to wait page, got %s", loc)
	}

	// Now follow wait page request
	waitReq := httptest.NewRequest(http.MethodGet, loc, nil)
	waitRec := httptest.NewRecorder()

	server.ServeHTTP(waitRec, waitReq)

	if waitRec.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK from wait page, got %d", waitRec.Code)
	}
	if waitRec.Body.String() != `{"async":"ok"}` {
		t.Fatalf("expected body `{\"async\":\"ok\"}`, got %s", waitRec.Body.String())
	}
}
