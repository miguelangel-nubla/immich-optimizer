//go:build e2e

package e2e

import (
	"net/http"
	"os"
	"testing"
	"time"
)

type face struct {
	ID         string `json:"id"`
	SourceType string `json:"sourceType"`
	Person     *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"person"`
}

func (u *user) faces(t *testing.T, assetID string) []face {
	t.Helper()
	var out []face
	must(t, u.key, http.MethodGet, "/faces?id="+assetID, nil, &out)
	return out
}

// settledFaces waits until Immich's face detection has run on the asset and
// the result stopped changing, then returns the faces.
func (u *user) settledFaces(t *testing.T, assetID string, timeout time.Duration) []face {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []face
	stableSince := time.Now()
	for {
		now := u.faces(t, assetID)
		same := len(now) == len(last)
		for i := 0; same && i < len(now); i++ {
			same = now[i].ID == last[i].ID && (now[i].Person == nil) == (last[i].Person == nil)
		}
		if !same {
			last, stableSince = now, time.Now()
		}
		detected := false
		for _, f := range now {
			if f.SourceType == "machine-learning" && f.Person != nil {
				detected = true
			}
		}
		if detected && time.Since(stableSince) > 20*time.Second {
			return now
		}
		if time.Now().After(deadline) {
			return now
		}
		time.Sleep(2 * time.Second)
	}
}

// With Immich's own face detection and recognition running: the person an
// original belongs to must still be on its converted copy, without duplicate
// faces, once Immich has processed the new file too.
func TestLibraryKeepsPeopleWithMachineLearning(t *testing.T) {
	if os.Getenv("E2E_ML") == "" {
		t.Skip("needs Immich's machine learning: run with E2E_ML=1 scripts/e2e-test.sh")
	}

	// Only face recognition is needed; a person is created from a single face.
	var cfg map[string]any
	must(t, admin(t), http.MethodGet, "/system-config", nil, &cfg)
	ml := cfg["machineLearning"].(map[string]any)
	ml["enabled"] = true
	for _, k := range []string{"clip", "duplicateDetection", "ocr"} {
		if m, ok := ml[k].(map[string]any); ok {
			m["enabled"] = false
		}
	}
	fr := ml["facialRecognition"].(map[string]any)
	fr["enabled"], fr["minFaces"] = true, 1
	must(t, admin(t), http.MethodPut, "/system-config", cfg, nil)

	u := newUser(t)
	samples := []string{"faces/face1.jpg", "faces/face2.jpg", "faces/face3.jpg"}
	var originals []asset
	for _, s := range samples {
		status, out := u.upload(t, immichURL, mediaDir+"/"+s, s[len("faces/"):], nil)
		if status != http.StatusCreated {
			t.Fatalf("uploading %s: %d %s", s, status, out["_body"])
		}
		originals = append(originals, u.waitReady(t, out["id"]))
	}

	// Immich detects the face in each and assigns a person (the first run
	// downloads the model, which can take minutes).
	personOf := map[string]string{}
	for _, a := range originals {
		faces := u.settledFaces(t, a.ID, 15*time.Minute)
		if len(faces) != 1 || faces[0].Person == nil {
			t.Fatalf("%s: Immich found %d face(s), want 1 with a person: %+v", a.OriginalFileName, len(faces), faces)
		}
		personOf[a.ID] = faces[0].Person.ID
	}
	// Give the person a name, as a user would.
	must(t, u.key, http.MethodPut, "/people/"+personOf[originals[0].ID], map[string]any{"name": "Mona"}, nil)

	code, out, log := runLibrary(t, u, sharedDir(t), map[string]string{"IO_LIBRARY_DRY_RUN": "false"})
	if code != 0 {
		t.Fatalf("library run exited %d:\n%s", code, out)
	}

	for _, a := range originals {
		var newID string
		for _, e := range log {
			if e["event"] == "replaced" && e["asset"] == a.ID {
				newID, _ = e["new_asset"].(string)
			}
		}
		if newID == "" {
			t.Errorf("%s was not replaced: %v\n%s", a.OriginalFileName, events(log, a.ID), out)
			continue
		}
		if !u.asset(t, a.ID).IsTrashed {
			t.Errorf("%s: original not in the trash", a.OriginalFileName)
		}
		// Right after the run the copy already belongs to the person...
		n := u.asset(t, newID)
		if len(n.People) != 1 || n.People[0].ID != personOf[a.ID] {
			t.Errorf("%s: people on the copy = %+v, want person %s", a.OriginalFileName, n.People, personOf[a.ID])
		}
		// ...and still does, exactly once, after Immich's own detection has
		// processed the new file.
		faces := u.settledFaces(t, newID, 5*time.Minute)
		assigned := 0
		for _, f := range faces {
			if f.Person != nil {
				assigned++
				if f.Person.ID != personOf[a.ID] {
					t.Errorf("%s: face assigned to another person %s", a.OriginalFileName, f.Person.ID)
				}
			}
		}
		if assigned != 1 {
			t.Errorf("%s: %d faces assigned to a person on the copy, want exactly 1: %+v", a.OriginalFileName, assigned, faces)
		}
	}

	// The named person still exists with its name.
	var p struct {
		Name string `json:"name"`
	}
	must(t, u.key, http.MethodGet, "/people/"+personOf[originals[0].ID], nil, &p)
	if p.Name != "Mona" {
		t.Errorf("person name = %q", p.Name)
	}
}
