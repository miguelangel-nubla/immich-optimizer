package library

import (
	"os"
	"path/filepath"
	"testing"
)

func writeLog(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestLogIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	x := newLogIndex(path, "")
	// A log that does not exist yet knows nothing.
	if err := x.refresh(); err != nil || x.isKnown() {
		t.Fatalf("missing log: %v, known %v", err, x.isKnown())
	}
	writeLog(t, path, `{"source":"optimizer","event":"started"}
{"source":"proxy","event":"uploaded","asset":"p1","outcome":"optimized","original_sha1":"AAAA"}
{"source":"proxy","event":"uploaded","asset":"p2","outcome":"unchanged"}
{"source":"watcher","event":"duplicate","asset":"w1","outcome":"optimized","original_sha1":"bbbb"}
not json at all
{"source":"library","event":"uploaded","asset":"o1","new_asset":"n1"}
{"source":"library","event":"replaced","asset":"o1","new_asset":"n1","original_sha1":"cccc"}
{"source":"library","event":"uploaded","asset":"o2","new_asset":"n2"}
{"source":"library","event":"uploaded","asset":"o3","new_asset":"n3"}
{"source":"library","event":"rolled_back","asset":"o3","new_asset":"n3"}
{"source":"library","event":"uploaded","asset":"o4","new_asset":"n4"}
{"source":"library","event":"rollback_failed","asset":"o4","new_asset":"n4"}
{"source":"library","event":"uploaded","asset":"o5"`)
	if err := x.refresh(); err != nil {
		t.Fatal(err)
	}
	if !x.isKnown() {
		t.Error("conversions in the log: it should know them")
	}
	for sha, want := range map[string]string{"aaaa": "p1", "bbbb": "w1", "cccc": "n1"} {
		if got, ok := x.convertedFrom(sha); !ok || got != want {
			t.Errorf("convertedFrom(%s) = %q, %v; want %q", sha, got, ok, want)
		}
	}
	// A conversion that gave nothing smaller is not a conversion.
	if len(x.originals) != 3 {
		t.Errorf("originals = %v", x.originals)
	}
	// n2 was never finished; n4's rollback failed: both are still there.
	if got := x.pendingIDs(); len(got) != 2 || !x.pending["n2"] || !x.pending["n4"] {
		t.Errorf("pending = %v", got)
	}

	// The last line was incomplete; finishing it is picked up.
	writeLog(t, path, `,"new_asset":"n5"}
{"source":"library","event":"finalized","asset":"o2","new_asset":"n2"}
`)
	if err := x.refresh(); err != nil {
		t.Fatal(err)
	}
	if !x.pending["n5"] || x.pending["n2"] || len(x.pending) != 2 {
		t.Errorf("after appending: pending = %v", x.pendingIDs())
	}

	// A log that was replaced by a shorter one is read again from the start.
	if err := os.WriteFile(path, []byte(`{"source":"library","event":"full_read"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	y := newLogIndex(path, "")
	if err := y.refresh(); err != nil || !y.isKnown() || len(y.originals) != 0 {
		t.Errorf("full read only: %v, known %v", err, y.isKnown())
	}
	x.offset = 1 << 20 // as if the file had been much longer before
	if err := x.refresh(); err != nil {
		t.Fatal(err)
	}
}

// Several users share one audit log: each one's index takes its own entries
// and those without a user (proxy and watcher conversions), never another
// user's.
func TestLogIndexPerUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	writeLog(t, path, `{"source":"proxy","event":"uploaded","asset":"p1","outcome":"optimized","original_sha1":"aaaa"}
{"source":"library","user":"alice","event":"replaced","asset":"o1","new_asset":"n1","original_sha1":"bbbb"}
{"source":"library","user":"bob","event":"replaced","asset":"o2","new_asset":"n2","original_sha1":"cccc"}
{"source":"library","user":"bob","event":"uploaded","asset":"o3","new_asset":"n3"}
{"source":"library","user":"bob","event":"full_read"}
`)
	alice, bob, carol := newLogIndex(path, "alice"), newLogIndex(path, "bob"), newLogIndex(path, "carol")
	for _, x := range []*logIndex{alice, bob, carol} {
		if err := x.refresh(); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := alice.convertedFrom("cccc"); ok {
		t.Error("alice sees bob's conversion")
	}
	if id, ok := alice.convertedFrom("bbbb"); !ok || id != "n1" {
		t.Error("alice does not see her own conversion")
	}
	if id, ok := bob.convertedFrom("aaaa"); !ok || id != "p1" {
		t.Error("a proxy conversion is not seen by everyone")
	}
	if len(alice.pendingIDs()) != 0 || len(bob.pendingIDs()) != 1 {
		t.Errorf("pending: alice %v, bob %v", alice.pendingIDs(), bob.pendingIDs())
	}
	// Bob's full read says nothing about Carol's library; the proxy
	// conversion is still something to go by for her.
	if !carol.isKnown() {
		t.Error("carol: the shared proxy conversion should count")
	}
}
