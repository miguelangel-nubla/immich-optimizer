package port

import (
	"context"
	"encoding/json"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// LibraryAPI is what library mode needs from Immich. Every call acts as the
// owner of the API key.
type LibraryAPI interface {
	// CurrentUserID returns the id of the API key's owner.
	CurrentUserID(ctx context.Context) (string, error)
	// ListAssets returns one page of the owner's assets (oldest first) and
	// the next page number, 0 when there is none.
	ListAssets(ctx context.Context, page int) ([]entity.LibraryAsset, int, error)
	// MatchingAssetIDs returns the ids of the assets that match a filter of
	// Immich's asset search, given as JSON.
	MatchingAssetIDs(ctx context.Context, filter json.RawMessage) ([]string, error)
	GetAsset(ctx context.Context, id string) (*entity.LibraryAsset, error)
	// GetMetadata returns all asset metadata items.
	GetMetadata(ctx context.Context, id string) ([]entity.MetadataItem, error)
	PutMetadata(ctx context.Context, id string, items []entity.MetadataItem) error
	// DownloadOriginal writes the stored file to dst and returns its
	// lowercase hex SHA-1.
	DownloadOriginal(ctx context.Context, id, dst string) (string, error)
	Upload(ctx context.Context, u entity.LibraryUpload) (*entity.LibraryUploadResult, error)
	// CopyAsset copies albums, shared links, stack, sidecar and favorite.
	CopyAsset(ctx context.Context, sourceID, targetID string) error
	UpdateAsset(ctx context.Context, id string, u entity.LibraryAssetUpdate) error
	Albums(ctx context.Context, assetID string) ([]entity.LibraryAlbum, error)
	// ActivityCount returns comments plus likes on an asset in an album.
	ActivityCount(ctx context.Context, albumID, assetID string) (int, error)
	TagAsset(ctx context.Context, assetID string, tagIDs []string) error
	Faces(ctx context.Context, assetID string) ([]entity.LibraryFace, error)
	CreateFace(ctx context.Context, assetID string, f entity.LibraryFace) error
	// SharedLinkIDs returns the individual shared links that contain the asset.
	SharedLinkIDs(ctx context.Context, assetID string) ([]string, error)
	AddToSharedLink(ctx context.Context, linkID, assetID string) error
	SetStackPrimary(ctx context.Context, stackID, assetID string) error
	// Edits returns the asset's non-destructive edits, in order.
	Edits(ctx context.Context, assetID string) ([]entity.LibraryEdit, error)
	SetEdits(ctx context.Context, assetID string, edits []entity.LibraryEdit) error
	// Trash moves assets to Immich's trash (restorable).
	Trash(ctx context.Context, ids []string) error
	// Delete removes assets permanently. Only used on assets this run
	// created itself, to roll a failed replacement back.
	Delete(ctx context.Context, ids []string) error
}
