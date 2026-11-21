package entity

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMarkerMergeIntoMetadata(t *testing.T) {
	m := &Marker{V: 1, SHA1: "a9993e364706816aba3e25717850c26c9cd0d89d", Size: 3, Task: "jpeg-xl", Source: "proxy"}

	// No client metadata: a one-item array.
	out, err := m.MergeIntoMetadata("")
	if err != nil {
		t.Fatal(err)
	}
	var items []struct {
		Key   string
		Value map[string]any
	}
	if err := json.Unmarshal([]byte(out), &items); err != nil || len(items) != 1 || items[0].Key != MarkerKey {
		t.Fatalf("unexpected output %s (%v)", out, err)
	}
	var back Marker
	raw, _ := json.Marshal(items[0].Value)
	if err := json.Unmarshal(raw, &back); err != nil || back.SHA1 != m.SHA1 || back.Task != "jpeg-xl" || back.Source != "proxy" {
		t.Errorf("marker value = %v (%v)", items[0].Value, err)
	}
	if strings.Contains(string(raw), "motion_photos") {
		t.Error("motion_photos must be omitted when empty")
	}

	// The client's items keep their content; a stale marker is replaced,
	// because Immich rejects the same key twice.
	mobile := `{"key":"mobile-app","value":{"iCloudId":"X:1", "n" : 1}}`
	out, err = m.MergeIntoMetadata(`[` + mobile + `,{"key":"immich-optimizer","value":{"v":0}}]`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `{"key":"mobile-app","value":{"iCloudId":"X:1","n":1}}`) {
		t.Errorf("client item was changed: %s", out)
	}
	if strings.Count(out, `"key":"immich-optimizer"`) != 1 || strings.Contains(out, `"v":0`) {
		t.Errorf("stale marker not replaced: %s", out)
	}

	if _, err := m.MergeIntoMetadata(`{"not":"an array"}`); err == nil {
		t.Error("expected an error for metadata that is not an array")
	}
}

// The record is read by other tools: its shape is part of the interface.
func TestMarkerShape(t *testing.T) {
	full := Marker{
		V: 1, SHA1: "aa", Size: 29293, OriginalName: "IMG_1.jpg",
		Task: "jpeg-xl", TaskCommand: `cjxl "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"`,
		ConfigSHA256: "cc", MotionPhotos: "drop_video", Source: "library", Optimizer: "v0.3.0",
		At: time.Date(2026, 11, 21, 10, 0, 0, 500, time.UTC), OutputSize: 21961,
		Replaces: "asset-1", State: MarkerStatePending,
	}
	got, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"original":{"sha1":"aa","size":29293,"name":"IMG_1.jpg"},` +
		`"conversion":{"at":"2026-11-21T10:00:00Z","source":"library","optimizer":"v0.3.0",` +
		`"task":{"name":"jpeg-xl","command":"cjxl \"{{.src_folder}}/{{.name}}.{{.extension}}\" \"{{.dst_folder}}/{{.name}}.jxl\"","motion_photos":"drop_video"},` +
		`"config":{"sha256":"cc"},"output":{"size":21961}},` +
		`"library":{"replaces":"asset-1","state":"pending"}}`
	if string(got) != want {
		t.Errorf("marker =\n%s\nwant\n%s", got, want)
	}
	var back Marker
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	full.At = full.At.Truncate(time.Second)
	if back != full {
		t.Errorf("read back\n%+v\nwant\n%+v", back, full)
	}

	// An original that was kept as it is.
	kept, _ := json.Marshal(Marker{V: 1, SHA1: "aa", Size: 5, Source: "library", Kept: true, Note: "no gain"})
	if want := `{"v":1,"original":{"sha1":"aa","size":5},"conversion":{"source":"library"},"kept":{"note":"no gain"}}`; string(kept) != want {
		t.Errorf("kept marker =\n%s\nwant\n%s", kept, want)
	}
	if err := json.Unmarshal(kept, &back); err != nil || !back.Kept || back.Note != "no gain" || back.State != "" {
		t.Errorf("kept marker read back as %+v (%v)", back, err)
	}
	// A very long command is cut, not refused.
	long, _ := json.Marshal(Marker{V: 1, Task: "t", TaskCommand: strings.Repeat("x", 3*maxMarkerCommand)})
	if len(long) > maxMarkerCommand+200 {
		t.Errorf("a long command was stored in full: %d bytes", len(long))
	}
}
