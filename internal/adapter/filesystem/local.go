package filesystem

import (
	"fmt"
	"os"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
)

// localFileSystem performs source-side file operations. Saving failed inputs
// is owned by the conversion engine's undone store (internal/adapter/undone).
type localFileSystem struct{}

func NewLocalFileSystem() port.FileSystem {
	return &localFileSystem{}
}

func (fs *localFileSystem) RemoveFile(filePath string) error {
	if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove file: %w", err)
	}
	return nil
}
