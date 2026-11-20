package entity

import (
	"encoding/json"
	"fmt"
	"time"
)

// MarkerKey is the Immich asset-metadata key under which the optimizer
// records where a converted asset came from.
const MarkerKey = "immich-optimizer"

// Marker is the provenance record written into Immich's asset metadata for
// every converted upload. It lets tools (and future sources) recognise a
// converted asset and find the checksum of its original without any state
// kept by the optimizer.
type Marker struct {
	// V is the schema version of this record.
	V int `json:"v"`
	// SHA1 is the lowercase hex SHA-1 of the original file, the checksum
	// Immich would have stored had the original been uploaded.
	SHA1 string `json:"sha1"`
	// Size is the size of the original file in bytes.
	Size int64 `json:"size"`
	// Task is the name of the task that produced the uploaded file.
	Task string `json:"task"`
	// Source is where the file came from (watcher, proxy).
	Source string `json:"source"`
	// MotionPhotos is set when the original was a motion photo that was
	// split for the task: "drop_video" or "drop_photo".
	MotionPhotos string `json:"motion_photos,omitempty"`
	// Optimizer is the immich-optimizer version that did the conversion.
	Optimizer string `json:"optimizer,omitempty"`
	// Replaces is the id of the asset this one replaced (library mode).
	Replaces string `json:"replaces,omitempty"`
	// State is MarkerStatePending while a library replacement is in
	// progress, and empty once the asset is final.
	State string `json:"state,omitempty"`
	// Kept is set on an original that library mode looked at and left as it
	// is, because converting it gave no size reduction.
	Kept bool `json:"kept,omitempty"`
	// Note says why an asset was kept.
	Note string `json:"note,omitempty"`
}

// MarkerStatePending marks a converted asset whose original has not been
// moved to the trash yet.
const MarkerStatePending = "pending"

type metadataItem struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// MergeIntoMetadata returns the value for the upload form's "metadata" field:
// existing (the client's own JSON array, or "") with this marker added. Any
// previous item with MarkerKey is replaced, because Immich rejects a request
// with the same key twice; every other item keeps its content.
func (m *Marker) MergeIntoMetadata(existing string) (string, error) {
	var items []metadataItem
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &items); err != nil {
			return "", fmt.Errorf("existing metadata is not a JSON array of items: %w", err)
		}
	}
	value, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	out := make([]metadataItem, 0, len(items)+1)
	for _, it := range items {
		if it.Key != MarkerKey {
			out = append(out, it)
		}
	}
	out = append(out, metadataItem{Key: MarkerKey, Value: value})
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Upload describes one file to upload to Immich.
type Upload struct {
	Path     string
	Filename string
	// ModTime is the date of the original file; it is sent as the asset's
	// file dates. Zero means "use Path's own modification time".
	ModTime time.Time
	// Marker is written into the asset's metadata when set.
	Marker *Marker
}

// MetadataItem is one key/value pair of Immich asset metadata.
type MetadataItem struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}
