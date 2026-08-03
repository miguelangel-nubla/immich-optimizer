package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

type localFileSystem struct {
	watchDir  string
	undoneDir string
}

func NewLocalFileSystem(watchDir, undoneDir string) port.FileSystem {
	return &localFileSystem{
		watchDir:  filepath.Clean(watchDir),
		undoneDir: filepath.Clean(undoneDir),
	}
}

func (fs *localFileSystem) MoveToUndone(filePath string) error {
	if err := fs.copyFileToUndone(filePath); err != nil {
		return err
	}
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("copied file to undone directory, but failed to remove original file %s: %w", filePath, err)
	}
	return nil
}

func (fs *localFileSystem) RemoveFile(filePath string) error {
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file: %w", err)
	}
	return nil
}

func (fs *localFileSystem) copyFileToUndone(filePath string) error {
	relPath, err := filepath.Rel(fs.watchDir, filePath)
	if err != nil {
		return fmt.Errorf("failed to get relative path: %w", err)
	}
	if strings.HasPrefix(relPath, "..") {
		return fmt.Errorf("file %s is outside watch directory %s", filePath, fs.watchDir)
	}

	destPath := filepath.Join(fs.undoneDir, relPath)
	destDir := filepath.Dir(destPath)

	if err := os.MkdirAll(destDir, 0750); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	src, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = dst.Close()
		}
	}()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	if err := dst.Sync(); err != nil {
		return fmt.Errorf("failed to sync destination file: %w", err)
	}

	closed = true
	if err := dst.Close(); err != nil {
		return fmt.Errorf("failed to close destination file: %w", err)
	}

	return nil
}
