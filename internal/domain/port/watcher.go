package port

import "github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"

type FileWatcher interface {
	Events() <-chan entity.FileEvent
	Start() error
	Stop() error
}
