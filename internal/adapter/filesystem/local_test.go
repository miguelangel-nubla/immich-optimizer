package filesystem

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

	fs := NewLocalFileSystem(watchDir, undoneDir).(*localFileSystem)

	// Test copyFileToUndone
	if err := fs.copyFileToUndone(testFilePath); err != nil {
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

	// Test MoveToUndone
	if err := fs.MoveToUndone(testFilePath); err != nil {
		t.Errorf("MoveToUndone failed: %v", err)
	}

	// Verify original file was removed after MoveToUndone
	if _, err := os.Stat(testFilePath); !os.IsNotExist(err) {
		t.Errorf("original file was NOT removed after MoveToUndone")
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

	fs := NewLocalFileSystem(watchDir, undoneDir).(*localFileSystem)

	err = fs.copyFileToUndone(tmpFile)
	if err == nil {
		t.Errorf("expected error when copying file outside watchDir, got nil")
	}
}
