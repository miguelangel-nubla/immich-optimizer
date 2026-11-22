// Package undone implements the conversion engine's store for inputs that
// could not be delivered. It is shared by every source (watcher, proxy, ...).
//
// Layout (flat, content addressed):
//
//	<root>/<sha1-hex>/original.file   the input, byte for byte, original mtime
//	<root>/<sha1-hex>/metadata.json   names, sources, failures (see metadata)
//
// The folder name is the lowercase hex SHA-1 of the content: the same digest
// Immich stores as an asset's checksum (Immich shows it base64 encoded, which
// metadata.json also records as sha1_base64), so an undone file can be looked
// up in Immich and vice versa. SHA-1 is used for identity only, never for
// security.
//
// Folders written by earlier versions (metadata version 2) are named after the
// SHA-256 of the content (64 hex characters). They are a different identity:
// this store never matches, updates, sweeps or removes them, so the same
// content failing again gets a new SHA-1 folder next to the legacy one.
// The same content always lands in the same folder: it is copied once and
// later failures only update metadata.json. Filenames and source directories
// are never used to build paths; they are recorded in metadata.json.
package undone

import (
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- content identity matching Immich's checksum, not security
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

const (
	// OriginalFile is the name of the saved input inside its hash folder.
	OriginalFile = "original.file"
	// MetadataFile is the name of the JSON metadata inside its hash folder.
	MetadataFile = "metadata.json"

	// LockFile is the advisory lock file at the store root. Every process
	// sharing the undone dir (for example a watcher and a proxy container)
	// takes it around folder publishing, metadata updates and the startup
	// sweep.
	LockFile = ".io-undone.lock"

	// staleTempAge is how old a top-level temp copy must be before the startup
	// sweep removes it. Copies run outside the lock and keep their mtime fresh
	// while being written, so an active copy of another process is never
	// mistaken for a leftover.
	staleTempAge = time.Hour

	// MetadataVersion is the metadata.json schema version. Version 3 switched
	// the content identity from SHA-256 to SHA-1 (folder name and "sha1").
	MetadataVersion = 3

	maxFailures    = 20  // failures kept in metadata.json (most recent)
	maxFilenames   = 100 // distinct original filenames kept
	maxNameBytes   = 1024
	maxErrorBytes  = 2 << 10
	maxOutputBytes = 4 << 10
)

// LocalStore saves failed inputs below root, one folder per distinct content.
type LocalStore struct {
	root    string
	version string
	log     *logger.Logger
	// mu (in process) and the LockFile flock (across processes) serialise
	// publishing folders and updating metadata, so concurrent saves of
	// identical content end with one folder and exact counts. The (slow) copy
	// happens outside them.
	mu         sync.Mutex
	lockWarned bool
	now        func() time.Time
	// copy is replaceable in tests to simulate mid-copy failures.
	copy func(dst io.Writer, src io.Reader) (int64, error)
}

var _ port.UndoneStore = (*LocalStore)(nil)

// CheckWritable creates dir if needed and verifies the current user can
// create files in it.
func CheckWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("undone_dir %q cannot be created by uid %d: %w", dir, os.Getuid(), err)
	}
	f, err := os.CreateTemp(dir, ".io-probe-*")
	if err != nil {
		return fmt.Errorf("undone_dir %q is not writable by uid %d (in Docker: give the host folder mounted there to that user, \"sudo chown %d <folder>\"): %w", dir, os.Getuid(), os.Getuid(), err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("undone_dir %q: unable to remove write probe: %w", dir, err)
	}
	return nil
}

// NewLocalStore validates root and sweeps temp entries left by an
// interrupted save.
func NewLocalStore(root, version string, log *logger.Logger) (*LocalStore, error) {
	if root == "" {
		return nil, errors.New("undone_dir must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("undone_dir %q: %w", root, err)
	}
	if err := CheckWritable(abs); err != nil {
		return nil, err
	}
	s := &LocalStore{
		root:    abs,
		version: version,
		log:     log,
		now:     time.Now,
		copy:    io.Copy,
	}
	s.sweepStaleTemps()
	return s, nil
}

// Root returns the absolute undone directory.
func (s *LocalStore) Root() string { return s.root }

func isTemp(name string) bool { return strings.HasPrefix(name, utils.UndoneTempPrefix) }

// isHashDir reports whether name looks like a content folder of this
// version (lowercase hex SHA-1). Legacy SHA-256 folders do not match.
func isHashDir(name string) bool {
	if len(name) != sha1.Size*2 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil && strings.ToLower(name) == name
}

// lock takes the in-process mutex and the cross-process advisory lock. If
// the filesystem does not support flock, it warns once and relies on the
// mutex alone.
func (s *LocalStore) lock() (unlock func()) {
	s.mu.Lock()
	release, err := lockFile(filepath.Join(s.root, LockFile))
	if err != nil {
		if !s.lockWarned {
			s.lockWarned = true
			s.warnf("Undone: cross-process lock unavailable (%v); do not share %s between processes", err, s.root)
		}
		return s.mu.Unlock
	}
	return func() {
		release()
		s.mu.Unlock()
	}
}

// sweepStaleTemps removes entries left by an interrupted save, under the
// lock: temp folders at the top level and temp metadata files inside content
// folders (both only exist while the lock is held), and top-level temp
// copies older than staleTempAge (copies are made outside the lock, possibly
// by another process). Nothing else is touched.
func (s *LocalStore) sweepStaleTemps() {
	unlock := s.lock()
	defer unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	remove := func(p string) {
		if err := os.RemoveAll(p); err == nil {
			s.printf("Removed stale undone temp entry %s", p)
		}
	}
	for _, e := range entries {
		p := filepath.Join(s.root, e.Name())
		switch {
		case isTemp(e.Name()):
			if !e.IsDir() {
				info, err := e.Info()
				if err != nil || s.now().Sub(info.ModTime()) < staleTempAge {
					continue
				}
			}
			remove(p)
		case e.IsDir() && isHashDir(e.Name()):
			inner, err := os.ReadDir(p)
			if err != nil {
				continue
			}
			for _, ie := range inner {
				if isTemp(ie.Name()) {
					remove(filepath.Join(p, ie.Name()))
				}
			}
		}
	}
}

func (s *LocalStore) record(sum string) *entity.UndoneRecord {
	dir := filepath.Join(s.root, sum)
	return &entity.UndoneRecord{
		Dir:          dir,
		Path:         filepath.Join(dir, OriginalFile),
		MetadataPath: filepath.Join(dir, MetadataFile),
		SHA1:         sum,
	}
}

// Save copies in.Path into the store (once per distinct content) and
// records the failure in that content's metadata.json. When the content is
// already stored, nothing is copied: only metadata.json is updated.
func (s *LocalStore) Save(in entity.ConvertInput, f entity.Failure) (*entity.UndoneRecord, error) {
	if f.At.IsZero() {
		f.At = s.now()
	}

	// Find the content's hash before copying anything, so a duplicate never
	// needs temp space (a large file failing again on a nearly full disk).
	known := strings.ToLower(in.SHA1)
	if !isHashDir(known) {
		if in.SHA1 != "" {
			s.warnf("Undone: ignoring malformed SHA-1 for %s", in.Filename)
		}
		var err error
		if known, err = hashFile(in.Path); err != nil {
			return nil, err
		}
	}
	if rec, ok, err := s.saveExisting(in, f, known); ok || err != nil {
		return rec, err
	}

	tmpPath, size, sum, err := s.copyToTemp(in)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmpPath) }() // no-op once renamed into place

	if sum != known {
		s.warnf("Undone: provided SHA-1 for %s does not match its content; using the computed one", in.Filename)
	}

	rec := s.record(sum)
	unlock := s.lock()
	defer unlock()

	// Only now, under the lock: until here the copy's fresh modification
	// time is what tells another process's startup sweep that it is in use.
	if !in.ModTime.IsZero() {
		if err := os.Chtimes(tmpPath, in.ModTime, in.ModTime); err != nil {
			s.warnf("Unable to preserve modification time on undone file: %v", err)
		}
	}

	st, err := os.Lstat(rec.Dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		published, err := s.publish(tmpPath, rec.Dir, s.newMetadata(in, f, sum, size))
		if err != nil {
			return nil, err
		}
		if published {
			rec.Occurrences = 1
			return rec, nil
		}
		// Another process published the same content first: update it.
	case err != nil:
		return nil, wrapIOErr("inspect undone folder", rec.Dir, err)
	case !st.IsDir():
		return nil, fmt.Errorf("undone: %s exists and is not a directory", rec.Dir)
	}

	// The folder exists. Restore original.file if someone removed it.
	if ost, err := os.Lstat(rec.Path); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(tmpPath, rec.Path); err != nil {
			return nil, wrapIOErr("save undone file", rec.Path, err)
		}
		syncDir(rec.Dir)
	} else if err != nil {
		return nil, wrapIOErr("inspect undone file", rec.Path, err)
	} else if !ost.Mode().IsRegular() {
		return nil, fmt.Errorf("undone: %s exists and is not a regular file", rec.Path)
	}
	s.recordDuplicate(rec, in, f, size)
	return rec, nil
}

// saveExisting records the failure without copying when sum's folder already
// holds original.file. ok is false when the content still has to be copied.
func (s *LocalStore) saveExisting(in entity.ConvertInput, f entity.Failure, sum string) (rec *entity.UndoneRecord, ok bool, err error) {
	rec = s.record(sum)
	unlock := s.lock()
	defer unlock()
	st, err := os.Lstat(rec.Path)
	if err != nil || !st.Mode().IsRegular() {
		// Missing, or something odd the copy path reports properly.
		return nil, false, nil
	}
	s.recordDuplicate(rec, in, f, st.Size())
	return rec, true, nil
}

// recordDuplicate adds one failure to an existing folder's metadata.json.
// The caller holds the lock.
func (s *LocalStore) recordDuplicate(rec *entity.UndoneRecord, in entity.ConvertInput, f entity.Failure, size int64) {
	m, err := readMetadata(rec.MetadataPath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.warnf("Undone metadata %s is unreadable, starting a new one: %v", rec.MetadataPath, err)
		}
		// The content predates (or lost) its metadata: count that earlier,
		// unknown occurrence without inventing a failure entry for it.
		m = &metadata{
			SHA1:          rec.SHA1,
			SHA1Base64:    hexToBase64(rec.SHA1),
			Size:          size,
			OriginalMTime: formatTime(in.ModTime),
			Occurrences:   1,
		}
	}
	m.Occurrences++
	s.addFailure(m, in, f)
	if err := writeMetadata(rec.MetadataPath, m); err != nil {
		s.warnf("Could not update undone metadata %s: %v", rec.MetadataPath, err)
	}
	rec.Duplicate = true
	rec.Occurrences = m.Occurrences
}

// hashFile returns the lowercase hex SHA-1 of the file at p.
func hashFile(p string) (string, error) {
	src, err := os.Open(p) // #nosec G304 -- the input being saved
	if err != nil {
		return "", fmt.Errorf("undone: unable to open input %s: %w", p, err)
	}
	defer src.Close()
	h := sha1.New() // #nosec G401 -- content identity, see package doc
	if _, err := io.Copy(h, src); err != nil {
		return "", fmt.Errorf("undone: unable to read input %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// publish assembles original.file and metadata.json in a temp folder and
// renames it to dir, so a complete-looking folder is never half written.
// It returns false (and no error) when dir appeared concurrently.
func (s *LocalStore) publish(tmpFile, dir string, m *metadata) (bool, error) {
	tmpDir := filepath.Join(s.root, utils.UndoneTempPrefix+randomHex(8)+".dir")
	if err := os.Mkdir(tmpDir, 0o750); err != nil {
		return false, wrapIOErr("create undone temp folder", tmpDir, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	if err := os.Rename(tmpFile, filepath.Join(tmpDir, OriginalFile)); err != nil {
		return false, wrapIOErr("save undone file", tmpDir, err)
	}
	if err := writeMetadata(filepath.Join(tmpDir, MetadataFile), m); err != nil {
		// The original is what matters; the metadata is recreated on the next
		// failure of the same content.
		s.warnf("Saving %s without metadata: %v", dir, err)
	}
	syncDir(tmpDir)
	if err := os.Rename(tmpDir, dir); err != nil {
		if _, statErr := os.Lstat(dir); statErr == nil {
			// Lost a race with another process. Put the copy back so the
			// caller can restore original.file if needed; cleanup drops it.
			_ = os.Rename(filepath.Join(tmpDir, OriginalFile), tmpFile)
			return false, nil
		}
		return false, wrapIOErr("publish undone folder", dir, err)
	}
	syncDir(s.root)
	cleanup = false
	return true, nil
}

func (s *LocalStore) copyToTemp(in entity.ConvertInput) (tmpPath string, size int64, sum string, err error) {
	src, err := os.Open(in.Path)
	if err != nil {
		return "", 0, "", fmt.Errorf("undone: unable to open input %s: %w", in.Path, err)
	}
	defer src.Close()

	tmpPath = filepath.Join(s.root, utils.UndoneTempPrefix+randomHex(8)+".tmp")
	// 0o666 so the umask applies exactly like os.Create: in the container the
	// file is owned by the container user with its default permissions.
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666) // #nosec G302
	if err != nil {
		return "", 0, "", wrapIOErr("create undone temp file", tmpPath, err)
	}
	fail := func(e error) (string, int64, string, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", 0, "", wrapIOErr("copy to undone", tmpPath, e)
	}

	h := sha1.New() // #nosec G401 -- content identity, see package doc
	size, err = s.copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", 0, "", wrapIOErr("copy to undone", tmpPath, err)
	}
	return tmpPath, size, hex.EncodeToString(h.Sum(nil)), nil
}

func (s *LocalStore) printf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

func (s *LocalStore) warnf(format string, args ...any) {
	if s.log != nil {
		s.log.Warnf(format, args...)
	}
}

func wrapIOErr(op, p string, err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("undone dir full: %s %s: %w", op, p, err)
	}
	return fmt.Errorf("undone: %s %s: %w", op, p, err)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func tailUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// ---- metadata.json ----

type metadataAttempt struct {
	Task       string `json:"task"`
	Command    string `json:"command,omitempty"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
}

type metadataFailure struct {
	Source           string            `json:"source"`
	OriginalFilename string            `json:"original_filename"`
	OriginalDir      string            `json:"original_dir,omitempty"`
	OriginalMTime    string            `json:"original_mtime,omitempty"`
	FailedAt         string            `json:"failed_at"`
	Stage            string            `json:"stage"`
	Error            string            `json:"error"`
	Attempts         []metadataAttempt `json:"attempts,omitempty"`
	Details          map[string]string `json:"details,omitempty"`
	OptimizerVersion string            `json:"optimizer_version,omitempty"`
}

// metadata is the schema of metadata.json. It never contains secrets.
type metadata struct {
	// Version is MetadataVersion. SHA1 is the lowercase hex SHA-1 of
	// original.file (the folder name); SHA1Base64 is the same digest base64
	// encoded, the form the Immich API shows as an asset's "checksum".
	Version           int      `json:"version"`
	SHA1              string   `json:"sha1"`
	SHA1Base64        string   `json:"sha1_base64"`
	Size              int64    `json:"size"`
	OriginalMTime     string   `json:"original_mtime,omitempty"`
	FirstFailedAt     string   `json:"first_failed_at"`
	LastFailedAt      string   `json:"last_failed_at"`
	Occurrences       int      `json:"occurrences"`
	OriginalFilenames []string `json:"original_filenames"`
	// Failures holds the most recent failures (at most maxFailures), oldest
	// first.
	Failures         []metadataFailure `json:"failures"`
	OptimizerVersion string            `json:"optimizer_version,omitempty"`
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *LocalStore) newMetadata(in entity.ConvertInput, f entity.Failure, sum string, size int64) *metadata {
	m := &metadata{
		SHA1:          sum,
		SHA1Base64:    hexToBase64(sum),
		Size:          size,
		OriginalMTime: formatTime(in.ModTime),
		Occurrences:   1,
	}
	s.addFailure(m, in, f)
	return m
}

func (s *LocalStore) addFailure(m *metadata, in entity.ConvertInput, f entity.Failure) {
	at := f.At.UTC().Format(time.RFC3339)
	m.Version = MetadataVersion
	if m.FirstFailedAt == "" {
		m.FirstFailedAt = at
	}
	m.LastFailedAt = at
	if m.OriginalMTime == "" {
		m.OriginalMTime = formatTime(in.ModTime)
	}
	if s.version != "" {
		m.OptimizerVersion = s.version
	}

	name := truncateUTF8(in.Filename, maxNameBytes)
	if name != "" && !contains(m.OriginalFilenames, name) && len(m.OriginalFilenames) < maxFilenames {
		m.OriginalFilenames = append(m.OriginalFilenames, name)
	}
	if m.OriginalFilenames == nil {
		m.OriginalFilenames = []string{}
	}

	fr := metadataFailure{
		Source:           string(in.Source),
		OriginalFilename: name,
		OriginalDir:      truncateUTF8(filepath.ToSlash(in.OriginalDir), maxNameBytes),
		OriginalMTime:    formatTime(in.ModTime),
		FailedAt:         at,
		Stage:            string(f.Stage),
		OptimizerVersion: s.version,
	}
	if fr.OriginalDir == "." {
		fr.OriginalDir = ""
	}
	if f.Err != nil {
		fr.Error = truncateUTF8(f.Err.Error(), maxErrorBytes)
	}
	for _, a := range f.Attempts {
		fr.Attempts = append(fr.Attempts, metadataAttempt{
			Task:       a.Task,
			Command:    truncateUTF8(a.Command, maxErrorBytes),
			ExitCode:   a.ExitCode,
			TimedOut:   a.TimedOut,
			DurationMS: a.Duration.Milliseconds(),
			Error:      truncateUTF8(a.Err, maxErrorBytes),
			OutputTail: tailUTF8(a.OutputTail, maxOutputBytes),
		})
	}
	if len(in.Details) > 0 {
		fr.Details = make(map[string]string, len(in.Details))
		for k, v := range in.Details {
			fr.Details[k] = truncateUTF8(v, 512)
		}
	}
	m.Failures = append(m.Failures, fr)
	if n := len(m.Failures); n > maxFailures {
		m.Failures = append([]metadataFailure(nil), m.Failures[n-maxFailures:]...)
	}
}

// hexToBase64 converts a hex digest to standard base64 (Immich's form).
func hexToBase64(h string) string {
	b, err := hex.DecodeString(h)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func readMetadata(p string) (*metadata, error) {
	data, err := os.ReadFile(p) // #nosec G304 -- path inside the undone store
	if err != nil {
		return nil, err
	}
	var m metadata
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Version != MetadataVersion {
		return nil, fmt.Errorf("unsupported metadata version %d", m.Version)
	}
	return &m, nil
}

// writeMetadata writes m atomically and durably: temp file in the same
// folder, fsync, rename, fsync of the folder.
func writeMetadata(p string, m *metadata) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(filepath.Dir(p), utils.UndoneTempPrefix+randomHex(8)+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666) // #nosec G302 G304 -- umask applies, like the saved input
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(p))
	return nil
}

// syncDir fsyncs a directory so renames into it survive a power loss. It is
// best effort: some platforms and filesystems do not support it.
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- a folder of the undone store
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
