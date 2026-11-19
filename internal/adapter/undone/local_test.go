package undone

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

func newStore(t *testing.T) (*LocalStore, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "undone")
	s, err := NewLocalStore(root, "vtest", nil)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	return s, s.Root()
}

func writeSrc(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func failure() entity.Failure {
	return entity.Failure{
		Stage: entity.StageConvert,
		Err:   errors.New("boom"),
		At:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Attempts: []entity.TaskAttempt{{
			Task: "heic2jxl", Command: "cjxl a b", ExitCode: 1, Duration: 1200 * time.Millisecond,
			Err: "exit status 1", OutputTail: "bad input",
		}},
	}
}

func readNoteT(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("note is not JSON: %v", err)
	}
	return m
}

func noTemps(t *testing.T, root string) {
	t.Helper()
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.HasPrefix(info.Name(), utils.UndoneTempPrefix) {
			t.Errorf("temp file left behind: %s", p)
		}
		return nil
	})
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// dirEntries lists root, failing the test on error.
func dirEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() == LockFile {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

func TestSaveBasic(t *testing.T) {
	s, root := newStore(t)
	src := writeSrc(t, "image-bytes")
	mtime := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	rec, err := s.Save(entity.ConvertInput{
		Path: src, Filename: "IMG_1234.HEIC", Source: entity.SourceProxy, ModTime: mtime,
		Details: map[string]string{"remote_addr": "1.2.3.4:5", "user_agent": "Immich"},
	}, failure())
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	dir := filepath.Join(root, sha("image-bytes"))
	if rec.Dir != dir || rec.Path != filepath.Join(dir, OriginalFile) || rec.MetadataPath != filepath.Join(dir, MetadataFile) ||
		rec.SHA256 != sha("image-bytes") || rec.Duplicate || rec.Occurrences != 1 {
		t.Fatalf("unexpected record %+v", rec)
	}
	if got := dirEntries(t, root); len(got) != 1 || got[0] != sha("image-bytes") {
		t.Fatalf("root must hold only the hash folder: %v", got)
	}
	if got := dirEntries(t, dir); len(got) != 2 || got[0] != MetadataFile || got[1] != OriginalFile {
		t.Fatalf("hash folder entries = %v", got)
	}
	got, _ := os.ReadFile(rec.Path)
	if string(got) != "image-bytes" {
		t.Fatalf("content = %q", got)
	}
	st, _ := os.Stat(rec.Path)
	if !st.ModTime().Equal(mtime) {
		t.Fatalf("mtime not applied: %v", st.ModTime())
	}
	umask := syscall.Umask(0)
	syscall.Umask(umask)
	if st.Mode().Perm() != os.FileMode(0o666&^umask) {
		t.Fatalf("mode = %v, want %v", st.Mode().Perm(), os.FileMode(0o666&^umask))
	}
	raw, _ := os.ReadFile(rec.MetadataPath)
	if !strings.Contains(string(raw), "\n  \"version\": 2,") {
		t.Fatalf("metadata must be pretty-printed with version 2: %s", raw)
	}
	n := readNoteT(t, rec.MetadataPath)
	for _, k := range []string{"version", "sha256", "size", "original_mtime", "first_failed_at", "last_failed_at", "occurrences", "original_filenames", "failures", "optimizer_version"} {
		if _, ok := n[k]; !ok {
			t.Errorf("metadata missing %q", k)
		}
	}
	if n["original_mtime"] != "2020-05-06T07:08:09Z" || n["sha256"] != sha("image-bytes") || n["size"].(float64) != 11 {
		t.Fatalf("metadata fields wrong: %v", n)
	}
	if names := n["original_filenames"].([]any); len(names) != 1 || names[0] != "IMG_1234.HEIC" {
		t.Fatalf("original_filenames = %v", names)
	}
	fl := n["failures"].([]any)[0].(map[string]any)
	for _, k := range []string{"source", "original_filename", "failed_at", "stage", "error", "attempts", "details"} {
		if _, ok := fl[k]; !ok {
			t.Errorf("failure missing %q", k)
		}
	}
	if fl["source"] != "proxy" || fl["stage"] != "convert" || fl["error"] != "boom" || fl["original_filename"] != "IMG_1234.HEIC" {
		t.Fatalf("failure fields wrong: %v", fl)
	}
	if _, ok := fl["original_dir"]; ok {
		t.Fatalf("original_dir must be omitted when unknown: %v", fl)
	}
	if details := fl["details"].(map[string]any); len(details) != 2 {
		t.Fatalf("details must contain only what was given: %v", details)
	}
	att := fl["attempts"].([]any)[0].(map[string]any)
	if att["task"] != "heic2jxl" || att["exit_code"].(float64) != 1 || att["duration_ms"].(float64) != 1200 {
		t.Fatalf("attempt wrong: %v", att)
	}
	noTemps(t, root)
}

// The watcher's relative directory is recorded, never mirrored on disk.
func TestSaveWatcherOriginalDirIsMetadataOnly(t *testing.T) {
	s, root := newStore(t)
	rec, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "a"), Filename: "x.jpg", Source: entity.SourceWatcher, OriginalDir: filepath.Join("sub", "dir")}, failure())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Path != filepath.Join(root, sha("a"), OriginalFile) {
		t.Fatalf("path = %s", rec.Path)
	}
	fl := readNoteT(t, rec.MetadataPath)["failures"].([]any)[0].(map[string]any)
	if fl["original_dir"] != "sub/dir" || fl["source"] != "watcher" {
		t.Fatalf("failure = %v", fl)
	}
}

// Names that are hostile as paths are only data.
func TestSaveHostileFilenamesAreOnlyMetadata(t *testing.T) {
	s, root := newStore(t)
	for i, name := range []string{"../../../etc/passwd", "", "..", "a\x00b\nc.jpg", strings.Repeat("é", 2000) + ".jpg"} {
		content := fmt.Sprintf("c%d", i)
		rec, err := s.Save(entity.ConvertInput{Path: writeSrc(t, content), Filename: name, Source: entity.SourceProxy, OriginalDir: "../x"}, failure())
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if rec.Path != filepath.Join(root, sha(content), OriginalFile) {
			t.Fatalf("%q escaped: %s", name, rec.Path)
		}
		n := readNoteT(t, rec.MetadataPath)
		fn := n["failures"].([]any)[0].(map[string]any)["original_filename"].(string)
		if len(fn) > maxNameBytes || strings.ToValidUTF8(fn, "?") != fn {
			t.Fatalf("filename not bounded/valid: %d bytes", len(fn))
		}
	}
	if got := dirEntries(t, root); len(got) != 5 {
		t.Fatalf("expected 5 hash folders, got %v", got)
	}
}

func TestSaveIdenticalDedup(t *testing.T) {
	s, root := newStore(t)
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f1 := failure()
	f1.At = first
	if _, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "same"), Filename: "a.jpg", Source: entity.SourceWatcher, OriginalDir: "d"}, f1); err != nil {
		t.Fatal(err)
	}
	f2 := failure()
	f2.Stage = entity.StageUpload
	f2.Err = errors.New("upload failed")
	rec, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "same"), Filename: "b.jpg", Source: entity.SourceProxy}, f2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "same"), Filename: "a.jpg", Source: entity.SourceProxy}, failure()); err != nil {
		t.Fatal(err)
	}
	if !rec.Duplicate || rec.Occurrences != 2 {
		t.Fatalf("expected duplicate with 2 occurrences, got %+v", rec)
	}
	if got := dirEntries(t, root); len(got) != 1 {
		t.Fatalf("expected one folder, got %v", got)
	}
	n := readNoteT(t, rec.MetadataPath)
	if n["occurrences"].(float64) != 3 || n["first_failed_at"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("metadata = %v", n)
	}
	if names := n["original_filenames"].([]any); len(names) != 2 || names[0] != "a.jpg" || names[1] != "b.jpg" {
		t.Fatalf("original_filenames must be de-duplicated: %v", names)
	}
	fl := n["failures"].([]any)
	if len(fl) != 3 || fl[1].(map[string]any)["stage"] != "upload" || fl[1].(map[string]any)["original_filename"] != "b.jpg" {
		t.Fatalf("failures = %v", fl)
	}
}

func TestSaveFailureListIsBounded(t *testing.T) {
	s, _ := newStore(t)
	var rec *entity.UndoneRecord
	for i := 0; i < maxFailures+5; i++ {
		f := failure()
		f.At = time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC)
		var err error
		rec, err = s.Save(entity.ConvertInput{Path: writeSrc(t, "x"), Filename: fmt.Sprintf("n%d.jpg", i), Source: entity.SourceProxy}, f)
		if err != nil {
			t.Fatal(err)
		}
	}
	n := readNoteT(t, rec.MetadataPath)
	fl := n["failures"].([]any)
	if len(fl) != maxFailures || n["occurrences"].(float64) != maxFailures+5 {
		t.Fatalf("failures=%d occurrences=%v", len(fl), n["occurrences"])
	}
	if fl[0].(map[string]any)["original_filename"] != "n5.jpg" || n["first_failed_at"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("oldest failures must be dropped, first_failed_at kept: %v %v", fl[0], n["first_failed_at"])
	}
	if names := n["original_filenames"].([]any); len(names) != maxFailures+5 {
		t.Fatalf("all distinct names must be kept: %d", len(names))
	}
}

func TestSaveConcurrentIdentical(t *testing.T) {
	s, root := newStore(t)
	src := writeSrc(t, strings.Repeat("z", 64<<10))
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Save(entity.ConvertInput{Path: src, Filename: fmt.Sprintf("v%d.mp4", i%3), Source: entity.SourceProxy}, failure())
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := dirEntries(t, root); len(got) != 1 {
		t.Fatalf("expected exactly one folder, got %v", got)
	}
	n := readNoteT(t, filepath.Join(root, sha(strings.Repeat("z", 64<<10)), MetadataFile))
	if n["occurrences"].(float64) != 20 || len(n["original_filenames"].([]any)) != 3 {
		t.Fatalf("occurrences = %v names = %v", n["occurrences"], n["original_filenames"])
	}
	noTemps(t, root)
}

func TestSaveDifferentContentSameNameNeverOverwrites(t *testing.T) {
	s, root := newStore(t)
	r1, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "first"), Filename: "IMG.jpg", Source: entity.SourceProxy}, failure())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "second"), Filename: "IMG.jpg", Source: entity.SourceProxy}, failure())
	if err != nil {
		t.Fatal(err)
	}
	if r1.Dir == r2.Dir || r2.Duplicate {
		t.Fatalf("different content must get its own folder: %+v %+v", r1, r2)
	}
	if b, _ := os.ReadFile(r1.Path); string(b) != "first" {
		t.Fatalf("first was overwritten: %q", b)
	}
	if b, _ := os.ReadFile(r2.Path); string(b) != "second" {
		t.Fatalf("second content = %q", b)
	}
	if got := dirEntries(t, root); len(got) != 2 {
		t.Fatalf("entries = %v", got)
	}
}

// A folder whose metadata is missing or unreadable still dedups and gets a
// fresh metadata.json that counts the earlier occurrence.
func TestSaveDedupWithoutMetadata(t *testing.T) {
	for _, meta := range []string{"", "{not json", `{"version": 1}`} {
		s, root := newStore(t)
		dir := filepath.Join(root, sha("legacy"))
		_ = os.MkdirAll(dir, 0o750)
		_ = os.WriteFile(filepath.Join(dir, OriginalFile), []byte("legacy"), 0o600)
		if meta != "" {
			_ = os.WriteFile(filepath.Join(dir, MetadataFile), []byte(meta), 0o600)
		}
		rec, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "legacy"), Filename: "old.jpg", Source: entity.SourceProxy}, failure())
		if err != nil || !rec.Duplicate || rec.Occurrences != 2 {
			t.Fatalf("meta %q: expected dedup, got %+v %v", meta, rec, err)
		}
		n := readNoteT(t, rec.MetadataPath)
		if n["version"].(float64) != MetadataVersion || n["occurrences"].(float64) != 2 {
			t.Fatalf("meta %q: metadata not rebuilt: %v", meta, n)
		}
		// Only the failure actually seen is recorded, once.
		if fl := n["failures"].([]any); len(fl) != 1 {
			t.Fatalf("meta %q: expected exactly one failure entry, got %d: %v", meta, len(fl), fl)
		}
	}
}

// If original.file was removed by hand, the next identical failure restores it.
func TestSaveRestoresMissingOriginal(t *testing.T) {
	s, _ := newStore(t)
	rec, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "data"), Filename: "a.jpg", Source: entity.SourceProxy}, failure())
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(rec.Path)
	rec2, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "data"), Filename: "a.jpg", Source: entity.SourceProxy}, failure())
	if err != nil || rec2.Occurrences != 2 {
		t.Fatalf("got %+v %v", rec2, err)
	}
	if b, _ := os.ReadFile(rec.Path); string(b) != "data" {
		t.Fatalf("original not restored: %q", b)
	}
}

func TestSaveReadOnlyRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	s, root := newStore(t)
	_ = os.Chmod(root, 0o500)
	defer os.Chmod(root, 0o750)
	if _, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "x"), Filename: "a.jpg", Source: entity.SourceProxy}, failure()); err == nil {
		t.Fatalf("expected error for read-only dir")
	}
	if got := dirEntries(t, root); len(got) != 0 {
		t.Fatalf("partial entries left: %v", got)
	}
}

func TestSaveMidCopyFailureLeavesNothing(t *testing.T) {
	s, root := newStore(t)
	s.copy = func(dst io.Writer, src io.Reader) (int64, error) {
		_, _ = dst.Write([]byte("par"))
		return 3, syscall.ENOSPC
	}
	_, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "xxxxxx"), Filename: "a.jpg", Source: entity.SourceProxy}, failure())
	if err == nil || !strings.Contains(err.Error(), "undone dir full") {
		t.Fatalf("expected disk-full error, got %v", err)
	}
	if got := dirEntries(t, root); len(got) != 0 {
		t.Fatalf("partial entries left: %v", got)
	}
}

func TestSaveMissingSource(t *testing.T) {
	s, root := newStore(t)
	if _, err := s.Save(entity.ConvertInput{Path: filepath.Join(t.TempDir(), "gone"), Filename: "a.jpg", Source: entity.SourceProxy}, failure()); err == nil {
		t.Fatalf("expected error")
	}
	if got := dirEntries(t, root); len(got) != 0 {
		t.Fatalf("partial entries left: %v", got)
	}
}

func TestSaveMetadataFailureStillSaves(t *testing.T) {
	s, root := newStore(t)
	rec, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "x"), Filename: "a.jpg", Source: entity.SourceProxy}, failure())
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(rec.MetadataPath)
	_ = os.Mkdir(rec.MetadataPath, 0o750) // a dir blocks the metadata rename
	rec2, err := s.Save(entity.ConvertInput{Path: writeSrc(t, "x"), Filename: "a.jpg", Source: entity.SourceProxy}, failure())
	if err != nil || rec2 == nil || !rec2.Duplicate {
		t.Fatalf("metadata failure must not fail the save: %+v %v", rec2, err)
	}
	if b, _ := os.ReadFile(rec.Path); string(b) != "x" {
		t.Fatalf("input not saved")
	}
	noTemps(t, root)
}

func TestNewLocalStoreSweepsStaleTemps(t *testing.T) {
	root := filepath.Join(t.TempDir(), "undone")
	hashDir := filepath.Join(root, sha("keep"))
	_ = os.MkdirAll(hashDir, 0o750)
	staleFile := filepath.Join(root, utils.UndoneTempPrefix+"dead.tmp")
	staleDir := filepath.Join(root, utils.UndoneTempPrefix+"dead.dir")
	staleMeta := filepath.Join(hashDir, utils.UndoneTempPrefix+"meta.tmp")
	keep := filepath.Join(hashDir, OriginalFile)
	other := filepath.Join(root, "user-notes", utils.UndoneTempPrefix+"x.tmp")
	_ = os.WriteFile(staleFile, []byte("x"), 0o600)
	old := time.Now().Add(-2 * staleTempAge)
	_ = os.Chtimes(staleFile, old, old)
	// A fresh top-level copy may belong to another process sharing the dir.
	active := filepath.Join(root, utils.UndoneTempPrefix+"active.tmp")
	_ = os.WriteFile(active, []byte("x"), 0o600)
	_ = os.MkdirAll(staleDir, 0o750)
	_ = os.WriteFile(filepath.Join(staleDir, OriginalFile), []byte("x"), 0o600)
	_ = os.WriteFile(staleMeta, []byte("x"), 0o600)
	_ = os.WriteFile(keep, []byte("keep"), 0o600)
	_ = os.MkdirAll(filepath.Dir(other), 0o750)
	_ = os.WriteFile(other, []byte("x"), 0o600)
	if _, err := NewLocalStore(root, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{staleFile, staleDir, staleMeta} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("stale temp %s not removed", p)
		}
	}
	for _, p := range []string{keep, other, active} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must not be removed", p)
		}
	}
}

// A crash between creating the temp folder and publishing it leaves only a
// temp entry, never a hash folder.
func TestPublishIsAllOrNothing(t *testing.T) {
	s, root := newStore(t)
	tmp := filepath.Join(root, utils.UndoneTempPrefix+"x.tmp")
	_ = os.WriteFile(tmp, []byte("x"), 0o600)
	target := filepath.Join(root, sha("x"))
	_ = os.WriteFile(target, []byte("in the way"), 0o600) // rename onto a file fails
	if ok, err := s.publish(tmp, target, &metadata{}); ok || err != nil {
		t.Fatalf("publish onto an existing entry must report a lost race: %v %v", ok, err)
	}
	if b, err := os.ReadFile(tmp); err != nil || string(b) != "x" {
		t.Fatalf("the copy must be handed back to the caller: %v", err)
	}
	if st, err := os.Stat(target); err != nil || st.IsDir() {
		t.Fatalf("target must be untouched: %v", err)
	}
	for _, name := range dirEntries(t, root) {
		if strings.HasSuffix(name, ".dir") {
			t.Fatalf("temp folder left: %s", name)
		}
	}
}

// A duplicate never needs temp space: with the content already stored, the
// copy is not even attempted (here it would fail as if the disk were full),
// whether the caller supplies the hash (proxy) or not (watcher).
func TestSaveDuplicateDoesNotCopy(t *testing.T) {
	for _, withHash := range []bool{true, false} {
		s, root := newStore(t)
		in := entity.ConvertInput{Path: writeSrc(t, "big"), Filename: "a.mp4", Source: entity.SourceProxy}
		if withHash {
			in.SHA256 = strings.ToUpper(sha("big"))
		}
		if _, err := s.Save(in, failure()); err != nil {
			t.Fatal(err)
		}
		s.copy = func(io.Writer, io.Reader) (int64, error) { return 0, syscall.ENOSPC }
		in.Filename = "b.mp4"
		rec, err := s.Save(in, failure())
		if err != nil || !rec.Duplicate || rec.Occurrences != 2 {
			t.Fatalf("hash=%v: duplicate must succeed without copying: %+v %v", withHash, rec, err)
		}
		n := readNoteT(t, rec.MetadataPath)
		if len(n["failures"].([]any)) != 2 || len(n["original_filenames"].([]any)) != 2 {
			t.Fatalf("hash=%v: metadata = %v", withHash, n)
		}
		noTemps(t, root)
	}
}

// Two stores (standing in for two processes) on one dir share the lock:
// concurrent identical saves still give one folder and exact counts.
func TestSaveTwoStoresSameDir(t *testing.T) {
	s1, root := newStore(t)
	s2, err := NewLocalStore(root, "vtest", nil)
	if err != nil {
		t.Fatal(err)
	}
	src := writeSrc(t, "shared")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(s *LocalStore) {
			defer wg.Done()
			if _, err := s.Save(entity.ConvertInput{Path: src, Filename: "a.jpg", Source: entity.SourceWatcher}, failure()); err != nil {
				t.Error(err)
			}
		}([]*LocalStore{s1, s2}[i%2])
	}
	wg.Wait()
	if got := dirEntries(t, root); len(got) != 1 {
		t.Fatalf("expected one folder, got %v", got)
	}
	if n := readNoteT(t, filepath.Join(root, sha("shared"), MetadataFile)); n["occurrences"].(float64) != 20 || len(n["failures"].([]any)) != maxFailures {
		t.Fatalf("occurrences = %v failures = %d", n["occurrences"], len(n["failures"].([]any)))
	}
	noTemps(t, root)
}
