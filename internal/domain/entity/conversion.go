package entity

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Source identifies where a file handed to the conversion engine came from.
type Source string

const (
	SourceWatcher Source = "watcher"
	SourceProxy   Source = "proxy"
	SourceLibrary Source = "library"
)

// ConvertInput describes one file handed to the conversion engine by a source.
type ConvertInput struct {
	// Path is a readable local file. The engine never modifies or deletes it.
	Path string
	// Filename is the user-facing original base name (watcher: file name;
	// proxy: the client's multipart filename). Its extension selects the task.
	Filename string
	Source   Source
	// OriginalDir is optional and informational only: the directory the file
	// came from (watcher: relative to the watch root; proxy: the directory
	// part of the client's multipart filename, if it sent one). It is
	// recorded in the undone metadata and never used to build a path.
	OriginalDir string
	// ModTime is optional; when set it is applied to the undone copy.
	ModTime time.Time
	// SHA1 is an optional precomputed lowercase hex SHA-1 of the content,
	// the same digest Immich stores as the asset checksum.
	SHA1 string
	// SkipUndone tells the engine not to save the input to undone when the
	// conversion fails. Library mode sets it: the original is still safe in
	// Immich, so a copy would only waste space.
	SkipUndone bool
	// Details are diagnostic key/values recorded in each failure of the undone
	// metadata.json.
	// They must NEVER contain secrets or authentication headers.
	Details map[string]string
}

type ConvertOutcome int

const (
	// OutcomeOptimized means the processed output is smaller and is delivered.
	OutcomeOptimized ConvertOutcome = iota
	// OutcomeUnchanged means conversion succeeded but gave no size reduction;
	// the original input is delivered.
	OutcomeUnchanged
)

func (o ConvertOutcome) String() string {
	switch o {
	case OutcomeOptimized:
		return "optimized"
	case OutcomeUnchanged:
		return "unchanged"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// ConvertResult is a deliverable conversion result.
type ConvertResult struct {
	Outcome      ConvertOutcome
	DeliverPath  string // processed output, or the input path when Unchanged
	DeliverName  string // name to deliver the file under
	OriginalSize int64
	OutputSize   int64
	// Release removes temporary work files. Never nil; idempotent.
	Release func()
	// Marker is the provenance record to store with the asset. It is nil
	// when the original itself is delivered (OutcomeUnchanged).
	Marker *Marker
}

type FailureStage string

const (
	StageNoTask  FailureStage = "no_task"
	StageConvert FailureStage = "convert"
	StageUpload  FailureStage = "upload"
)

// TaskAttempt records one task command run for a file.
type TaskAttempt struct {
	Task       string
	Command    string
	ExitCode   int // -1 if the error was not a process exit
	TimedOut   bool
	Duration   time.Duration
	Err        string
	OutputTail string // last few KiB of combined stdout/stderr
}

// Failure describes why a file could not be delivered.
type Failure struct {
	Stage    FailureStage
	Err      error
	Attempts []TaskAttempt
	At       time.Time
}

// UndoneRecord describes where a failed input was saved.
type UndoneRecord struct {
	Dir          string // absolute undone/<sha1> folder
	Path         string // Dir/original.file
	MetadataPath string // Dir/metadata.json
	SHA1         string // lowercase hex SHA-1 of the content (folder name)
	Duplicate    bool   // identical content was already saved
	Occurrences  int
}

var (
	ErrNoTask       = errors.New("no task configured for file extension")
	ErrCanceled     = errors.New("conversion canceled")
	ErrTaskTimeout  = errors.New("task timeout exceeded")
	ErrShuttingDown = errors.New("shutting down")
	ErrEmptyOutput  = errors.New("task produced empty output")
	ErrEmptyInput   = errors.New("input file is empty")
	// ErrAuditUnavailable means the audit log cannot be written: no work is
	// started without a record of it.
	ErrAuditUnavailable = errors.New("audit log cannot be written")
)

// ConversionError is returned by the engine for no-task and conversion
// failures. The input has already been handed to the undone store according
// to policy; Undone is nil when nothing was saved.
type ConversionError struct {
	Filename  string
	Failure   Failure
	Undone    *UndoneRecord
	UndoneErr error
}

func (e *ConversionError) Error() string {
	return fmt.Sprintf("processing %s failed: %v", e.Filename, e.Failure.Err)
}

func (e *ConversionError) Unwrap() error { return e.Failure.Err }

// ProcessError is returned by a MediaProcessor with per-task detail.
type ProcessError struct {
	Attempts []TaskAttempt
	Err      error
}

func (e *ProcessError) Error() string {
	if len(e.Attempts) == 0 {
		return e.Err.Error()
	}
	var b strings.Builder
	b.WriteString(e.Err.Error())
	for _, a := range e.Attempts {
		if a.Err == "" {
			continue
		}
		fmt.Fprintf(&b, "\ntask %s failed: %s", a.Task, a.Err)
		if a.Command != "" {
			fmt.Fprintf(&b, "\ncommand: %s", a.Command)
		}
		if a.OutputTail != "" {
			fmt.Fprintf(&b, "\noutput:\n%s", a.OutputTail)
		}
	}
	return b.String()
}

func (e *ProcessError) Unwrap() error { return e.Err }
