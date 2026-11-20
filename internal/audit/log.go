// Package audit keeps the persistent record of asset operations.
package audit

import (
	"encoding/json"
	"fmt"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one line of the audit log: a JSON object per event, appended and
// flushed to disk as it happens, so the file is a complete record of what the
// optimizer did to which asset, even after a crash.
//
// Only operations on assets are recorded: a file converted and uploaded, a
// file refused or failed and saved to undone, an existing asset replaced.
// Ordinary proxied traffic and client mistakes are not.
type Entry struct {
	Time time.Time `json:"time"`
	// Run identifies one start of the optimizer.
	Run string `json:"run"`
	// Source is the mode the event comes from: proxy, watcher or library.
	Source string `json:"source"`
	Event  string `json:"event"`
	// Asset is the Immich asset the event is about: the one created by an
	// upload, or the original that library mode works on.
	Asset string `json:"asset,omitempty"`
	File  string `json:"file,omitempty"`
	// NewAsset is the converted copy that replaces Asset (library mode).
	NewAsset string `json:"new_asset,omitempty"`
	// Outcome says what was delivered: "optimized" or "unchanged".
	Outcome        string   `json:"outcome,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	OriginalBytes  int64    `json:"original_bytes,omitempty"`
	ConvertedBytes int64    `json:"converted_bytes,omitempty"`
	OriginalSHA1   string   `json:"original_sha1,omitempty"`
	ConvertedSHA1  string   `json:"converted_sha1,omitempty"`
	Task           string   `json:"task,omitempty"`
	Differences    []string `json:"differences,omitempty"`
	// Undone is the undone folder a failed input was saved to.
	Undone  string `json:"undone,omitempty"`
	Summary any    `json:"summary,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// Log appends entries to the audit log file.
type Log struct {
	mu   sync.Mutex
	f    *os.File
	run  string
	now  func() time.Time
	path string
	// failed is the error of the last write, nil when it succeeded.
	failed error
}

// Open opens (creating if needed) the log file for appending.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creating audit log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening audit log: %w", err)
	}
	now := time.Now().UTC()
	return &Log{f: f, run: now.Format("20060102T150405Z"), now: time.Now, path: path}, nil
}

func (j *Log) Path() string { return j.path }

// Write appends one entry and syncs it to disk. A nil Log records nothing
// (tests). The audit log is mandatory: after a failed write, Check reports
// the log as unavailable and no source starts new work until a write
// succeeds again.
func (j *Log) Write(e Entry) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.failed = j.write(e)
	return j.failed
}

// Check reports whether the log can be written. When the last write failed
// it tries again with a marker entry, so the log recovers by itself once the
// cause (a full disk, a lost mount) is gone.
func (j *Log) Check() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failed == nil {
		return nil
	}
	j.failed = j.write(Entry{Source: "optimizer", Event: "audit_recovered", Detail: "after: " + j.failed.Error()})
	return j.failed
}

func (j *Log) write(e Entry) error {
	e.Time = j.now().UTC()
	e.Run = j.run
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("writing audit log: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("syncing audit log: %w", err)
	}
	return nil
}

func (j *Log) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

// Delivered builds the entry for a file that reached Immich through a source:
// event "uploaded", or "duplicate" when Immich already had those bytes.
func Delivered(source entity.Source, filename string, res *entity.ConvertResult, up entity.UploadResult) Entry {
	e := Entry{Source: string(source), Event: "uploaded", Asset: up.AssetID, File: filename}
	if up.Duplicate {
		e.Event = "duplicate"
	}
	if res != nil {
		e.Outcome = res.Outcome.String()
		e.OriginalBytes, e.ConvertedBytes = res.OriginalSize, res.OutputSize
		if res.Marker != nil {
			e.OriginalSHA1, e.Task = res.Marker.SHA1, res.Marker.Task
		}
	}
	return e
}
