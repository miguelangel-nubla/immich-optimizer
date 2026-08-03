//go:build !linux

package watcher

import (
	"fmt"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

func NewInotifyWatcher(watchDir string, log *logger.Logger, bufferSize int) (port.FileWatcher, error) {
	return nil, fmt.Errorf("inotify watcher is only supported on Linux")
}
