package filesystem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.jpg")
	if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := NewLocalFileSystem()
	if err := fs.RemoveFile(p); err != nil {
		t.Fatalf("RemoveFile failed: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("file still exists after RemoveFile")
	}
	// Removing a missing file is not an error.
	if err := fs.RemoveFile(p); err != nil {
		t.Fatalf("RemoveFile on missing file: %v", err)
	}
}
