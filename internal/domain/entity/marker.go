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
//
// In Immich it is a nested JSON object, see MarshalJSON. The fields here are
// flat because that is how the code uses them.
type Marker struct {
	// V is the schema version of the record.
	V int

	// SHA1 is the lowercase hex SHA-1 of the original file, the checksum
	// Immich would have stored had the original been uploaded.
	SHA1 string
	// Size is the size of the original file in bytes.
	Size int64
	// OriginalName is the original's file name.
	OriginalName string

	// Task is the name of the task that produced the uploaded file.
	Task string
	// TaskCommand is that task's command as it is written in the tasks
	// file, placeholders included: a task's name stays when its command is
	// changed.
	TaskCommand string
	// ConfigSHA256 identifies the content of the folder the tasks file is
	// in (the file itself, and the scripts and presets its commands call).
	// Two records with the same value were made with the same configuration.
	ConfigSHA256 string
	// MotionPhotos is set when the original was a motion photo that was
	// split for the task: "drop_video" or "drop_photo".
	MotionPhotos string
	// Source is where the file came from (watcher, proxy, library).
	Source string
	// Optimizer is the immich-optimizer version that did the conversion.
	Optimizer string
	// At is when the conversion was made.
	At time.Time
	// OutputSize is the size of the converted file in bytes.
	OutputSize int64

	// Replaces is the id of the asset this one replaced (library mode).
	Replaces string
	// State is MarkerStatePending while a library replacement is in
	// progress, and empty once the asset is final.
	State string

	// Kept is set on an original that library mode looked at and left as it
	// is, because converting it gave no size reduction.
	Kept bool
	// Note says why an asset was kept.
	Note string
}

// MarkerVersion is the schema version written.
const MarkerVersion = 1

// maxMarkerCommand bounds the command text kept in a record.
const maxMarkerCommand = 4096

// The record as it is stored:
//
//	{
//	  "v": 1,
//	  "original":   {"sha1": "...", "size": 29293, "name": "IMG_1234.jpg"},
//	  "conversion": {
//	    "at": "2026-11-21T10:00:00Z", "source": "proxy", "optimizer": "v0.3.0",
//	    "task":   {"name": "jpeg-xl", "command": "cjxl ...", "motion_photos": "drop_video"},
//	    "config": {"sha256": "..."},
//	    "output": {"size": 21961}
//	  },
//	  "library": {"replaces": "<asset id>", "state": "pending"},
//	  "kept":    {"note": "..."}
//	}
type markerJSON struct {
	V          int               `json:"v"`
	Original   *markerOriginal   `json:"original,omitempty"`
	Conversion *markerConversion `json:"conversion,omitempty"`
	Library    *markerLibrary    `json:"library,omitempty"`
	Kept       *markerKept       `json:"kept,omitempty"`
}

type markerOriginal struct {
	SHA1 string `json:"sha1"`
	Size int64  `json:"size"`
	Name string `json:"name,omitempty"`
}

type markerConversion struct {
	At        *time.Time    `json:"at,omitempty"`
	Source    string        `json:"source,omitempty"`
	Optimizer string        `json:"optimizer,omitempty"`
	Task      *markerTask   `json:"task,omitempty"`
	Config    *markerConfig `json:"config,omitempty"`
	Output    *markerOutput `json:"output,omitempty"`
}

type markerTask struct {
	Name         string `json:"name"`
	Command      string `json:"command,omitempty"`
	MotionPhotos string `json:"motion_photos,omitempty"`
}

type markerConfig struct {
	SHA256 string `json:"sha256"`
}

type markerOutput struct {
	Size int64 `json:"size"`
}

type markerLibrary struct {
	Replaces string `json:"replaces,omitempty"`
	State    string `json:"state,omitempty"`
}

type markerKept struct {
	Note string `json:"note,omitempty"`
}

// MarshalJSON writes the record in its nested form.
func (m Marker) MarshalJSON() ([]byte, error) {
	out := markerJSON{V: MarkerVersion, Original: &markerOriginal{SHA1: m.SHA1, Size: m.Size, Name: m.OriginalName}}
	c := &markerConversion{Source: m.Source, Optimizer: m.Optimizer}
	if !m.At.IsZero() {
		at := m.At.UTC().Truncate(time.Second)
		c.At = &at
	}
	if m.Task != "" || m.TaskCommand != "" || m.MotionPhotos != "" {
		cmd := m.TaskCommand
		if len(cmd) > maxMarkerCommand {
			cmd = cmd[:maxMarkerCommand]
		}
		c.Task = &markerTask{Name: m.Task, Command: cmd, MotionPhotos: m.MotionPhotos}
	}
	if m.ConfigSHA256 != "" {
		c.Config = &markerConfig{SHA256: m.ConfigSHA256}
	}
	if m.OutputSize > 0 {
		c.Output = &markerOutput{Size: m.OutputSize}
	}
	out.Conversion = c
	if m.Replaces != "" || m.State != "" {
		out.Library = &markerLibrary{Replaces: m.Replaces, State: m.State}
	}
	if m.Kept {
		out.Kept = &markerKept{Note: m.Note}
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads a record.
func (m *Marker) UnmarshalJSON(data []byte) error {
	var in markerJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*m = Marker{V: in.V}
	if o := in.Original; o != nil {
		m.SHA1, m.Size, m.OriginalName = o.SHA1, o.Size, o.Name
	}
	if c := in.Conversion; c != nil {
		m.Source, m.Optimizer = c.Source, c.Optimizer
		if c.At != nil {
			m.At = *c.At
		}
		if c.Task != nil {
			m.Task, m.TaskCommand, m.MotionPhotos = c.Task.Name, c.Task.Command, c.Task.MotionPhotos
		}
		if c.Config != nil {
			m.ConfigSHA256 = c.Config.SHA256
		}
		if c.Output != nil {
			m.OutputSize = c.Output.Size
		}
	}
	if l := in.Library; l != nil {
		m.Replaces, m.State = l.Replaces, l.State
	}
	if in.Kept != nil {
		m.Kept, m.Note = true, in.Kept.Note
	}
	return nil
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

// UploadResult is what Immich answered to an upload.
type UploadResult struct {
	AssetID string
	// Duplicate means Immich already had these exact bytes as AssetID.
	Duplicate bool
}

// MetadataItem is one key/value pair of Immich asset metadata.
type MetadataItem struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}
