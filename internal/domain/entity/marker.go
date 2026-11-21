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
	// OutputSize, OutputName and OutputSHA1 describe the converted file.
	// OutputSHA1 is the checksum Immich has for the asset.
	OutputSize int64
	OutputName string
	OutputSHA1 string

	// Earlier are the versions between the original and the latest
	// conversion, oldest first: each a file and how it was made. They are
	// there when an asset was converted again.
	Earlier []MarkerStep

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

// MarkerStep is one version of a file: the file, and how it was made from
// the version before it.
type MarkerStep struct {
	SHA1         string
	Size         int64
	Name         string
	At           time.Time
	Source       string
	Optimizer    string
	Task         string
	TaskCommand  string
	MotionPhotos string
	ConfigSHA256 string
}

// Latest returns the latest conversion as a step.
func (m Marker) Latest() MarkerStep {
	return MarkerStep{SHA1: m.OutputSHA1, Size: m.OutputSize, Name: m.OutputName, At: m.At, Source: m.Source,
		Optimizer: m.Optimizer, Task: m.Task, TaskCommand: m.TaskCommand, MotionPhotos: m.MotionPhotos, ConfigSHA256: m.ConfigSHA256}
}

// Then returns the record of a file converted again from the one m
// describes, by the conversion next records: the original stays, m's
// latest conversion becomes an earlier version, and next's is the latest.
// Library state is not carried over.
func (m Marker) Then(next Marker) Marker {
	out := next
	out.V = MarkerVersion
	out.SHA1, out.Size, out.OriginalName = m.SHA1, m.Size, m.OriginalName
	out.Earlier = append(append([]MarkerStep(nil), m.Earlier...), m.Latest())
	out.Replaces, out.State, out.Kept, out.Note = "", "", false, ""
	return out
}

// MarkerVersion is the schema version written.
const MarkerVersion = 1

// maxMarkerCommand bounds the command text kept in a record.
const maxMarkerCommand = 4096

// The record as it is stored: the versions of the file, oldest first. The
// first is the original, as it was before any conversion; each one after it
// was made from the one before. The last is the file as it is in Immich.
//
//	{
//	  "v": 1,
//	  "versions": [
//	    {"sha1": "...", "size": 29293, "name": "IMG_1234.jpg"},
//	    {"sha1": "...", "size": 21961, "name": "IMG_1234.jxl",
//	     "at": "2026-11-21T10:00:00Z", "source": "proxy", "optimizer": "v0.4.0",
//	     "task":   {"name": "jpeg-xl", "command": "cjxl ...", "motion_photos": "drop_video"},
//	     "config": {"sha256": "..."}}
//	  ],
//	  "library": {"replaces": "<asset id>", "state": "pending"},
//	  "kept":    {"note": "...", "at": "...", "optimizer": "v0.4.0"}
//	}
//
// An asset that was left as it is has only its original, and "kept".
type markerJSON struct {
	V        int             `json:"v"`
	Versions []markerVersion `json:"versions,omitempty"`
	Library  *markerLibrary  `json:"library,omitempty"`
	Kept     *markerKept     `json:"kept,omitempty"`
}

type markerVersion struct {
	SHA1      string        `json:"sha1,omitempty"`
	Size      int64         `json:"size,omitempty"`
	Name      string        `json:"name,omitempty"`
	At        *time.Time    `json:"at,omitempty"`
	Source    string        `json:"source,omitempty"`
	Optimizer string        `json:"optimizer,omitempty"`
	Task      *markerTask   `json:"task,omitempty"`
	Config    *markerConfig `json:"config,omitempty"`
}

type markerTask struct {
	Name         string `json:"name"`
	Command      string `json:"command,omitempty"`
	MotionPhotos string `json:"motion_photos,omitempty"`
}

type markerConfig struct {
	SHA256 string `json:"sha256"`
}

type markerLibrary struct {
	Replaces string `json:"replaces,omitempty"`
	State    string `json:"state,omitempty"`
}

type markerKept struct {
	Note      string     `json:"note,omitempty"`
	At        *time.Time `json:"at,omitempty"`
	Optimizer string     `json:"optimizer,omitempty"`
}

func jsonTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	t = t.UTC().Truncate(time.Second)
	return &t
}

func (st MarkerStep) toJSON() markerVersion {
	v := markerVersion{SHA1: st.SHA1, Size: st.Size, Name: st.Name, At: jsonTime(st.At), Source: st.Source, Optimizer: st.Optimizer}
	if st.Task != "" || st.TaskCommand != "" || st.MotionPhotos != "" {
		cmd := st.TaskCommand
		if len(cmd) > maxMarkerCommand {
			cmd = cmd[:maxMarkerCommand]
		}
		v.Task = &markerTask{Name: st.Task, Command: cmd, MotionPhotos: st.MotionPhotos}
	}
	if st.ConfigSHA256 != "" {
		v.Config = &markerConfig{SHA256: st.ConfigSHA256}
	}
	return v
}

func (v markerVersion) step() MarkerStep {
	st := MarkerStep{SHA1: v.SHA1, Size: v.Size, Name: v.Name, Source: v.Source, Optimizer: v.Optimizer}
	if v.At != nil {
		st.At = *v.At
	}
	if v.Task != nil {
		st.Task, st.TaskCommand, st.MotionPhotos = v.Task.Name, v.Task.Command, v.Task.MotionPhotos
	}
	if v.Config != nil {
		st.ConfigSHA256 = v.Config.SHA256
	}
	return st
}

// MarshalJSON writes the record in its stored form.
func (m Marker) MarshalJSON() ([]byte, error) {
	out := markerJSON{V: MarkerVersion}
	out.Versions = append(out.Versions, markerVersion{SHA1: m.SHA1, Size: m.Size, Name: m.OriginalName})
	if m.Kept {
		out.Kept = &markerKept{Note: m.Note, At: jsonTime(m.At), Optimizer: m.Optimizer}
	} else {
		for _, st := range m.Earlier {
			out.Versions = append(out.Versions, st.toJSON())
		}
		out.Versions = append(out.Versions, m.Latest().toJSON())
	}
	if m.Replaces != "" || m.State != "" {
		out.Library = &markerLibrary{Replaces: m.Replaces, State: m.State}
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
	if len(in.Versions) > 0 {
		o := in.Versions[0]
		m.SHA1, m.Size, m.OriginalName = o.SHA1, o.Size, o.Name
	}
	if n := len(in.Versions); n > 1 {
		for _, v := range in.Versions[1 : n-1] {
			m.Earlier = append(m.Earlier, v.step())
		}
		last := in.Versions[n-1].step()
		m.OutputSHA1, m.OutputSize, m.OutputName, m.At = last.SHA1, last.Size, last.Name, last.At
		m.Source, m.Optimizer, m.Task, m.TaskCommand = last.Source, last.Optimizer, last.Task, last.TaskCommand
		m.MotionPhotos, m.ConfigSHA256 = last.MotionPhotos, last.ConfigSHA256
	}
	if l := in.Library; l != nil {
		m.Replaces, m.State = l.Replaces, l.State
	}
	if k := in.Kept; k != nil {
		m.Kept, m.Note, m.Optimizer = true, k.Note, k.Optimizer
		if k.At != nil {
			m.At = *k.At
		}
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
