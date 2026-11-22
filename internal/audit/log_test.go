package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

func TestLogAppendsOneJSONObjectPerLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "audit.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(Entry{Source: "proxy", Event: "uploaded", Asset: "a1", File: "x.jpg"}); err != nil {
		t.Fatal(err)
	}
	l.Close()

	// A later start appends, it never truncates.
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(Entry{Source: "library", Event: "replaced", Asset: "a1", NewAsset: "a2"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if err := l.Write(Entry{Event: "late"}); err == nil {
		t.Error("writing to a closed log must fail, so callers can stop")
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var got []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line is not JSON: %q", sc.Text())
		}
		got = append(got, e)
	}
	if len(got) != 2 || got[0].Event != "uploaded" || got[1].NewAsset != "a2" {
		t.Fatalf("entries = %+v", got)
	}
	for _, e := range got {
		if e.Time.IsZero() || e.Run == "" {
			t.Errorf("entry without time or run: %+v", e)
		}
	}
	var nilLog *Log
	if err := nilLog.Write(Entry{Event: "x"}); err != nil {
		t.Errorf("a nil log must record nothing without failing: %v", err)
	}
}

func TestDelivered(t *testing.T) {
	res := &entity.ConvertResult{Outcome: entity.OutcomeOptimized, OriginalSize: 100, OutputSize: 40,
		Marker: &entity.Marker{SHA1: "abc", Task: "jpeg-xl"}}
	e := Delivered(entity.SourceWatcher, "a.jpg", res, entity.UploadResult{AssetID: "id1"})
	if e.Event != "uploaded" || e.Source != "watcher" || e.Asset != "id1" || e.Outcome != "optimized" ||
		e.OriginalBytes != 100 || e.ConvertedBytes != 40 || e.OriginalSHA1 != "abc" || e.Task != "jpeg-xl" {
		t.Errorf("entry = %+v", e)
	}
	e = Delivered(entity.SourceProxy, "a.jpg", &entity.ConvertResult{Outcome: entity.OutcomeUnchanged}, entity.UploadResult{AssetID: "id1", Duplicate: true})
	if e.Event != "duplicate" || e.Outcome != "unchanged" || e.Task != "" {
		t.Errorf("entry = %+v", e)
	}
}

// After a failed write the log reports itself unavailable, and recovers by
// itself once it can be written again.
func TestCheckReportsAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Check(); err != nil {
		t.Fatalf("a fresh log must be healthy: %v", err)
	}
	good := l.f
	// Break it: writes now fail.
	broken, _ := os.Open(path) // read-only handle
	l.f = broken
	if err := l.Write(Entry{Event: "x"}); err == nil {
		t.Fatal("write on a read-only file succeeded")
	}
	if err := l.Check(); err == nil {
		t.Error("Check must fail while the log cannot be written")
	}
	// Repair it: Check writes a marker and reports healthy again.
	l.f = good
	if err := l.Check(); err != nil {
		t.Errorf("Check did not recover: %v", err)
	}
	if err := l.Check(); err != nil {
		t.Errorf("still unhealthy: %v", err)
	}
	l.Close()
	broken.Close()
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte(`"event":"audit_recovered"`)) {
		t.Errorf("no recovery marker in the log: %s", data)
	}
}

// A log that ends in part of a line (a crash mid-write) does not swallow the
// next entry: it starts on a line of its own.
func TestLogAfterATornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(path, []byte(`{"event":"uploaded","asset":"a1"}`+"\n"+`{"event":"upl`), 0o640); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(Entry{Source: "library", Event: "replaced", Asset: "a2"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Write(Entry{Source: "library", Event: "replaced", Asset: "a3"}); err != nil {
		t.Fatal(err)
	}
	l.Close()
	b, _ := os.ReadFile(path)
	var assets []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			assets = append(assets, e.Asset)
		}
	}
	if len(assets) != 3 || assets[0] != "a1" || assets[1] != "a2" || assets[2] != "a3" {
		t.Errorf("readable entries %v, want [a1 a2 a3]\n%s", assets, b)
	}
}
