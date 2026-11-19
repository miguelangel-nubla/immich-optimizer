package port

import "github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"

// UndoneStore keeps copies of inputs that could not be delivered.
type UndoneStore interface {
	Save(in entity.ConvertInput, f entity.Failure) (*entity.UndoneRecord, error)
}
