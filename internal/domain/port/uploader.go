package port

import "context"

type AssetUploader interface {
	UploadAssetWithFilename(ctx context.Context, filePath, filename string) error
}
