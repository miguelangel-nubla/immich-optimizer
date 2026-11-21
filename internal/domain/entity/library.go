package entity

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrAssetNotFound is returned when Immich has no such asset (or the API key
// may not read it).
var ErrAssetNotFound = errors.New("asset not found")

// LibraryAsset is the part of an Immich asset that library mode reads,
// compares and carries over to the converted copy.
type LibraryAsset struct {
	ID               string
	OwnerID          string
	LibraryID        string // set for external-library assets
	Type             string // IMAGE, VIDEO...
	OriginalFileName string
	Checksum         string // lowercase hex SHA-1 of the stored file
	FileCreatedAt    time.Time
	FileModifiedAt   time.Time
	LocalDateTime    time.Time
	IsFavorite       bool
	IsTrashed        bool
	IsEdited         bool
	IsOffline        bool
	Visibility       string // timeline, archive, hidden, locked
	Duration         string
	LivePhotoVideoID string
	// Ready reports that Immich finished reading the file: metadata is
	// extracted and the thumbnail exists.
	Ready bool

	Size int64

	// Metadata Immich extracted from the file or the user edited.
	DateTimeOriginal string
	TimeZone         string
	Latitude         *float64
	Longitude        *float64
	Description      string
	Rating           *float64
	Width            int
	Height           int
	Make             string
	Model            string
	LensModel        string
	Orientation      string
	ProjectionType   string

	TagIDs    []string
	PersonIDs []string

	StackID        string
	StackPrimaryID string
}

// LibraryFace is one face box assigned to a person.
type LibraryFace struct {
	PersonID    string
	X1, Y1      int
	X2, Y2      int
	ImageWidth  int
	ImageHeight int
}

// LibraryAlbum is an album an asset belongs to.
type LibraryAlbum struct {
	ID     string
	Name   string
	Shared bool
}

// LibraryAssetUpdate holds the user-editable fields to set on an asset. Nil
// fields are left alone.
type LibraryAssetUpdate struct {
	Description      *string
	Rating           *float64
	DateTimeOriginal *string
	Latitude         *float64
	Longitude        *float64
	IsFavorite       *bool
	Visibility       *string
	// LivePhotoVideoID links a photo to the video it is paired with.
	LivePhotoVideoID *string
}

// LibraryEdit is one non-destructive edit (crop, rotate, mirror) of an asset.
type LibraryEdit struct {
	Action     string          `json:"action"`
	Parameters json.RawMessage `json:"parameters"`
}

// LibraryUpload is a converted file uploaded to replace an existing asset.
type LibraryUpload struct {
	Path           string
	Filename       string
	FileCreatedAt  time.Time
	FileModifiedAt time.Time
	IsFavorite     bool
	Visibility     string
	// Metadata is the complete asset metadata to store: the original's own
	// items plus the optimizer marker.
	Metadata []MetadataItem
}

// LibraryUploadResult is Immich's answer to an upload.
type LibraryUploadResult struct {
	ID        string
	Duplicate bool // Immich already had these exact bytes as asset ID
}

// LibraryUser is the Immich user an API key belongs to. ID never changes;
// name and email can.
type LibraryUser struct {
	ID    string
	Name  string
	Email string
}
