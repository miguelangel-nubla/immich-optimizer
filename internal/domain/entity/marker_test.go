package entity

import (
	"encoding/json"
	"reflect"
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
		ConfigSHA256: "cc", MotionPhotos: "drop_video", GainMaps: "drop", Source: "proxy", Optimizer: "v0.3.0",
		At:         time.Date(2026, 11, 21, 10, 0, 0, 500, time.UTC),
		OutputSize: 21961, OutputName: "IMG_1.jxl", OutputSHA1: "bb",
		Replaces: "asset-1", State: MarkerStatePending,
	}
	got, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":1,"versions":[{"sha1":"aa","size":29293,"name":"IMG_1.jpg"},` +
		`{"sha1":"bb","size":21961,"name":"IMG_1.jxl","at":"2026-11-21T10:00:00Z","source":"proxy","optimizer":"v0.3.0",` +
		`"task":{"name":"jpeg-xl","command":"cjxl \"{{.src_folder}}/{{.name}}.{{.extension}}\" \"{{.dst_folder}}/{{.name}}.jxl\"","motion_photos":"drop_video","gain_maps":"drop"},` +
		`"config":{"sha256":"cc"}}],` +
		`"library":{"replaces":"asset-1","state":"pending"}}`
	if string(got) != want {
		t.Errorf("marker =\n%s\nwant\n%s", got, want)
	}
	var back Marker
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatal(err)
	}
	full.At = full.At.Truncate(time.Second)
	if !reflect.DeepEqual(back, full) {
		t.Errorf("read back\n%+v\nwant\n%+v", back, full)
	}

	// Converted again: the original stays first, the earlier conversion
	// moves into the middle, the new one is last.
	again := full.Then(Marker{V: 1, SHA1: "bb", Size: 21961, OriginalName: "IMG_1.jxl", Task: "smaller", Source: "library",
		Optimizer: "v0.4.0", At: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC), OutputSize: 15000, OutputName: "IMG_1.jxl", OutputSHA1: "cc"})
	if again.SHA1 != "aa" || again.State != "" || again.Replaces != "" || len(again.Earlier) != 1 || again.Earlier[0].SHA1 != "bb" ||
		again.Earlier[0].Task != "jpeg-xl" || again.OutputSHA1 != "cc" || again.Task != "smaller" {
		t.Fatalf("converted again: %+v", again)
	}
	got, _ = json.Marshal(again)
	var v struct {
		Versions []struct {
			SHA1   string `json:"sha1"`
			Source string `json:"source"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(got, &v); err != nil || len(v.Versions) != 3 || v.Versions[0].SHA1 != "aa" ||
		v.Versions[1].SHA1 != "bb" || v.Versions[1].Source != "proxy" || v.Versions[2].SHA1 != "cc" {
		t.Errorf("versions of the converted-again record: %s", got)
	}
	var back2 Marker
	if err := json.Unmarshal(got, &back2); err != nil || !reflect.DeepEqual(back2, again) {
		t.Errorf("converted-again record read back as\n%+v\nwant\n%+v", back2, again)
	}

	// An original that was kept as it is: only the original, and why.
	kept, _ := json.Marshal(Marker{V: 1, SHA1: "aa", Size: 5, Source: "library", Optimizer: "v0.4.0", Kept: true, Note: "no gain",
		At: time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)})
	if want := `{"v":1,"versions":[{"sha1":"aa","size":5}],"kept":{"note":"no gain","at":"2026-11-21T10:00:00Z","optimizer":"v0.4.0"}}`; string(kept) != want {
		t.Errorf("kept marker =\n%s\nwant\n%s", kept, want)
	}
	if err := json.Unmarshal(kept, &back); err != nil || !back.Kept || back.Note != "no gain" || back.State != "" || back.SHA1 != "aa" {
		t.Errorf("kept marker read back as %+v (%v)", back, err)
	}
	// A very long command is cut, not refused.
	long, _ := json.Marshal(Marker{V: 1, Task: "t", TaskCommand: strings.Repeat("x", 3*maxMarkerCommand)})
	if len(long) > maxMarkerCommand+200 {
		t.Errorf("a long command was stored in full: %d bytes", len(long))
	}
}
