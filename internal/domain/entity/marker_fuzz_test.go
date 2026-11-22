package entity

import (
	"encoding/json"
	"strings"
	"testing"
)

// A marker is read from Immich, where anyone with the asset can write any
// JSON under its key. Reading must never crash, and what is read must be
// written back the same way it reads again.
func FuzzMarkerJSON(f *testing.F) {
	f.Add([]byte(`{"v":1,"versions":[{"sha1":"aa","size":3,"name":"a.jpg"},{"sha1":"bb","size":2,"task":{"name":"jxl"}}],"library":{"replaces":"x","state":"pending"}}`))
	f.Add([]byte(`{"v":1,"versions":[{"sha1":"aa","size":3}],"kept":{"note":"no gain"}}`))
	f.Add([]byte(`{"v":1,"versions":[{},{},{},{}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var m Marker
		if json.Unmarshal(data, &m) != nil {
			return
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("a marker that was read cannot be written: %v", err)
		}
		var again Marker
		if err := json.Unmarshal(out, &again); err != nil {
			t.Fatalf("what was written cannot be read: %v\n%s", err, out)
		}
		out2, _ := json.Marshal(again)
		if string(out) != string(out2) {
			t.Fatalf("not stable:\n%s\n%s", out, out2)
		}
	})
}

// The client's own metadata field of an upload is merged with the marker.
// Whatever the client sent, the result has the marker exactly once, or the
// merge says it cannot be done.
func FuzzMergeIntoMetadata(f *testing.F) {
	f.Add(``)
	f.Add(`[]`)
	f.Add(`[{"key":"mobile-app","value":{"iCloudId":"x"}}]`)
	f.Add(`[{"key":"immich-optimizer","value":{}},{"key":"immich-optimizer","value":1}]`)
	f.Add(`{"key":"x"}`)
	f.Fuzz(func(t *testing.T, existing string) {
		m := &Marker{V: MarkerVersion, SHA1: "aa", Size: 1}
		out, err := m.MergeIntoMetadata(existing)
		if err != nil {
			return
		}
		var items []struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(out), &items); err != nil {
			t.Fatalf("the merged field is not a list: %v\n%s", err, out)
		}
		n := 0
		for _, it := range items {
			if it.Key == MarkerKey {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d markers in %s", n, out)
		}
		if strings.TrimSpace(existing) == "" && len(items) != 1 {
			t.Fatalf("an empty field became %s", out)
		}
	})
}
