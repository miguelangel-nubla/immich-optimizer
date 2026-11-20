package library

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one line of the library log: a JSON object per event, appended and
// flushed to disk as it happens, so the file is a complete record of what
// library mode did to which asset even after a crash.
type Entry struct {
	Time  time.Time `json:"time"`
	Run   string    `json:"run"`
	Event string    `json:"event"`
	// Asset is the id of the original asset the event is about.
	Asset string `json:"asset,omitempty"`
	File  string `json:"file,omitempty"`
	// NewAsset is the id of the converted copy.
	NewAsset       string   `json:"new_asset,omitempty"`
	Detail         string   `json:"detail,omitempty"`
	OriginalBytes  int64    `json:"original_bytes,omitempty"`
	ConvertedBytes int64    `json:"converted_bytes,omitempty"`
	OriginalSHA1   string   `json:"original_sha1,omitempty"`
	ConvertedSHA1  string   `json:"converted_sha1,omitempty"`
	Task           string   `json:"task,omitempty"`
	Differences    []string `json:"differences,omitempty"`
	Summary        *Summary `json:"summary,omitempty"`
	DryRun         bool     `json:"dry_run,omitempty"`
}

// Journal appends entries to the persistent library log.
type Journal struct {
	mu   sync.Mutex
	f    *os.File
	run  string
	now  func() time.Time
	path string
}

// OpenJournal opens (creating if needed) the log file for appending.
func OpenJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("creating library log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("opening library log: %w", err)
	}
	now := time.Now().UTC()
	return &Journal{f: f, run: now.Format("20060102T150405Z"), now: time.Now, path: path}, nil
}

func (j *Journal) Path() string { return j.path }

// Write appends one entry and syncs it to disk. A log that cannot be written
// is an error for the caller: library mode never changes Immich without a
// record of it.
func (j *Journal) Write(e Entry) error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	e.Time = j.now().UTC()
	e.Run = j.run
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("writing library log: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("syncing library log: %w", err)
	}
	return nil
}

func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}
