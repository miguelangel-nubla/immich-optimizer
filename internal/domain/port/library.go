package port

import (
	"context"

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
	// Tags returns the owner's tags, by value ("parent/child") to id.
	Tags(ctx context.Context) (map[string]string, error)
	// UpsertTags creates the tags that do not exist yet and returns the ids
	// of all of them, by value.
	UpsertTags(ctx context.Context, values []string) (map[string]string, error)
	// TaggedAssetIDs returns the owner's assets (in the timeline or the
	// archive, not in the trash) that have the tag.
	TaggedAssetIDs(ctx context.Context, tagID string) ([]string, error)
	// TagAssets and UntagAssets add a tag to assets or remove it.
	TagAssets(ctx context.Context, tagID string, assetIDs []string) error
	UntagAssets(ctx context.Context, tagID string, assetIDs []string) error
	GetAsset(ctx context.Context, id string) (*entity.LibraryAsset, error)
	// GetMetadata returns all asset metadata items.
	GetMetadata(ctx context.Context, id string) ([]entity.MetadataItem, error)
	PutMetadata(ctx context.Context, id string, items []entity.MetadataItem) error
	// DeleteMetadata removes one metadata item from an asset.
	DeleteMetadata(ctx context.Context, id, key string) error
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
	// MemoryIDs returns, for every asset that is in a memory, the memories
	// it is in.
	MemoryIDs(ctx context.Context) (map[string][]string, error)
	AddToMemory(ctx context.Context, memoryID, assetID string) error
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

// LibraryEvents delivers Immich's live events for the API key's user.
type LibraryEvents interface {
	// Listen connects, delivers events to h and reconnects after any
	// failure, until ctx ends.
	Listen(ctx context.Context, h LibraryEventHandlers)
}

// LibraryEventHandlers are called from Listen's goroutine; they must not
// block.
type LibraryEventHandlers struct {
	// Connected is called on every (re)connection: events may have been
	// missed while there was none.
	Connected func()
	// AssetUpdated is called when an asset changed, with the values of its
	// tags ("parent/child").
	AssetUpdated func(assetID string, tags []string)
	// Uploaded is called for a new asset, with its SHA-1 as Immich gives
	// it (base64).
	Uploaded func(assetID, checksum string)
}
