package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyAndMoveToUndone(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "iuo_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	watchDir := filepath.Join(tempDir, "watch")
	undoneDir := filepath.Join(tempDir, "undone")
	subDir := filepath.Join(watchDir, "subfolder")

	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create subDir: %v", err)
	}

	testFilePath := filepath.Join(subDir, "test.jpg")
	content := []byte("test image data")
	if err := os.WriteFile(testFilePath, content, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	// Test copyFileToUndone
	if err := copyFileToUndone(testFilePath, watchDir, undoneDir); err != nil {
		t.Errorf("copyFileToUndone failed: %v", err)
	}

	expectedUndonePath := filepath.Join(undoneDir, "subfolder", "test.jpg")
	undoneContent, err := os.ReadFile(expectedUndonePath)
	if err != nil {
		t.Errorf("failed to read undone file: %v", err)
	}
	if string(undoneContent) != string(content) {
		t.Errorf("expected content %q, got %q", string(content), string(undoneContent))
	}

	// Verify original file still exists after copyFileToUndone
	if _, err := os.Stat(testFilePath); os.IsNotExist(err) {
		t.Errorf("original file was removed after copyFileToUndone")
	}

	// Test moveToUndone
	if err := moveToUndone(testFilePath, watchDir, undoneDir); err != nil {
		t.Errorf("moveToUndone failed: %v", err)
	}

	// Verify original file was removed after moveToUndone
	if _, err := os.Stat(testFilePath); !os.IsNotExist(err) {
		t.Errorf("original file was NOT removed after moveToUndone")
	}
}

func TestCopyFileToUndoneOutsideWatchDir(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "iuo_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	watchDir := filepath.Join(tempDir, "watch")
	undoneDir := filepath.Join(tempDir, "undone")
	tmpFile := filepath.Join(tempDir, "outside", "file.tmp")

	if err := os.MkdirAll(filepath.Dir(tmpFile), 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	os.WriteFile(tmpFile, []byte("data"), 0644)

	err = copyFileToUndone(tmpFile, watchDir, undoneDir)
	if err == nil {
		t.Errorf("expected error when copying file outside watchDir, got nil")
	}
}

func TestHumanReadableSize(t *testing.T) {
	tests := []struct {
		size     int64
		expected string
	}{
		{500, "500 bytes"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1048576, "1.00 MB"},
		{1073741824, "1.00 GB"},
		{1099511627776, "1.00 TB"},
	}

	for _, tt := range tests {
		result := humanReadableSize(tt.size)
		if result != tt.expected {
			t.Errorf("humanReadableSize(%d) = %q; expected %q", tt.size, result, tt.expected)
		}
	}
}

func TestNormalizeExtension(t *testing.T) {
	tests := []struct {
		ext      string
		expected string
	}{
		{".JPG", "jpg"},
		{"png", "png"},
		{".tar.gz", "tar.gz"},
		{"", ""},
	}

	for _, tt := range tests {
		result := normalizeExtension(tt.ext)
		if result != tt.expected {
			t.Errorf("normalizeExtension(%q) = %q; expected %q", tt.ext, result, tt.expected)
		}
	}
}

func TestShouldProcessExtension(t *testing.T) {
	tasks := []Task{
		{Extensions: []string{"jpg", "jpeg"}},
		{Extensions: []string{"png"}},
	}

	tests := []struct {
		ext      string
		expected bool
	}{
		{".JPG", true},
		{"jpeg", true},
		{".png", true},
		{".gif", false},
		{"", false},
	}

	for _, tt := range tests {
		result := shouldProcessExtension(tt.ext, tasks)
		if result != tt.expected {
			t.Errorf("shouldProcessExtension(%q) = %v; expected %v", tt.ext, result, tt.expected)
		}
	}
}

func TestTrimSuffixCaseInsensitive(t *testing.T) {
	tests := []struct {
		str      string
		suffix   string
		expected string
	}{
		{"image.JPG", ".jpg", "image"},
		{"image.jpg", ".JPG", "image"},
		{"IMAGE.PNG", ".png", "IMAGE"},
		{"image.gif", ".jpg", "image.gif"},
		{"nosuffix", ".txt", "nosuffix"},
	}

	for _, tt := range tests {
		result := trimSuffixCaseInsensitive(tt.str, tt.suffix)
		if result != tt.expected {
			t.Errorf("trimSuffixCaseInsensitive(%q, %q) = %q; expected %q", tt.str, tt.suffix, result, tt.expected)
		}
	}
}
