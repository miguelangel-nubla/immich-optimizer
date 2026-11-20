package port

import (
	"context"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

type AssetUploader interface {
	UploadAsset(ctx context.Context, u entity.Upload) error
}
