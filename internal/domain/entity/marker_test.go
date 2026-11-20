package entity

import (
	"encoding/json"
	"strings"
	"testing"
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
	if items[0].Value["sha1"] != m.SHA1 || items[0].Value["task"] != "jpeg-xl" {
		t.Errorf("marker value = %v", items[0].Value)
	}
	if _, has := items[0].Value["motion_photos"]; has {
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
