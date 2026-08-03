package port

import "context"

type AssetUploader interface {
	UploadAsset(ctx context.Context, filePath string) error
	UploadAssetWithFilename(ctx context.Context, filePath, filename string) error
}
