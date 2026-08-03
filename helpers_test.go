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
