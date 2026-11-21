//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"
)

// Real clips from phones, from Immich's own public-domain test assets, at a
// fixed commit. They are too big to keep in this repository.
const testAssets = "https://raw.githubusercontent.com/immich-app/test-assets/2228520c31c6cbc8df69698e129e308b9fc59c96/videos/"

// phoneClip downloads one of them once and returns its path.
func phoneClip(t *testing.T, name string) string {
	t.Helper()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, "immich-optimizer", "test-assets")
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(testAssets + name)
	if err != nil {
		t.Fatalf("downloading %s: %v", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("downloading %s: status %d", name, resp.StatusCode)
	}
	f, err := os.Create(path + ".part")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		t.Fatalf("downloading %s: %v", name, err)
	}
	f.Close()
	if err := os.Rename(path+".part", path); err != nil {
		t.Fatal(err)
	}
	return path
}

// videoFacts is what Immich reads from a video and shows to the user.
type videoFacts struct {
	ID            string
	Duration      float64 `json:"-"`
	RawDuration   any     `json:"duration"`
	LocalDateTime string  `json:"localDateTime"`
	FileCreatedAt string  `json:"fileCreatedAt"`
	Mime          string  `json:"originalMimeType"`
	Width         float64 `json:"width"`
	Height        float64 `json:"height"`
	Exif          struct {
		DateTimeOriginal string   `json:"dateTimeOriginal"`
		TimeZone         *string  `json:"timeZone"`
		Latitude         *float64 `json:"latitude"`
		Longitude        *float64 `json:"longitude"`
		Width            *float64 `json:"exifImageWidth"`
		Height           *float64 `json:"exifImageHeight"`
		Orientation      *string  `json:"orientation"`
		Make             *string  `json:"make"`
		Model            *string  `json:"model"`
		Size             int64    `json:"fileSizeInByte"`
	} `json:"exifInfo"`
}

func (u *user) videoFacts(t *testing.T, id string) videoFacts {
	t.Helper()
	u.waitReady(t, id)
	var raw json.RawMessage
	must(t, u.key, http.MethodGet, "/assets/"+id, nil, &raw)
	var v videoFacts
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	v.ID = id
	return v
}

func ptr[T any](p *T) string {
	if p == nil {
		return "(none)"
	}
	return fmt.Sprint(*p)
}

func near(a, b *float64) bool {
	return a == nil && b == nil || a != nil && b != nil && math.Abs(*a-*b) < 0.0005
}

// convertWith runs the task a bundled profile has for the file, as the
// optimizer would, and returns the file it wrote. It does not go through the
// optimizer, which keeps the original when the result is not smaller: phones
// already store video compactly, and what is checked here is the result.
func convertWith(t *testing.T, profile, src string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(configDir, profile, "tasks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Tasks []struct {
			Name       string   `yaml:"name"`
			Command    string   `yaml:"command"`
			Extensions []string `yaml:"extensions"`
		} `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(src)), ".")
	command := ""
	for _, task := range doc.Tasks {
		if contains(task.Extensions, ext) {
			command = task.Command
			break
		}
	}
	if strings.TrimSpace(command) == "" {
		t.Fatalf("profile %s has no command for .%s", profile, ext)
	}
	work := sharedDir(t)
	for _, d := range []string{"src", "dst"} {
		if err := os.Mkdir(filepath.Join(work, d), 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(filepath.Join(work, d), 0o777)
	}
	in, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "src", "clip."+ext), in, 0o644); err != nil {
		t.Fatal(err)
	}
	var cmd strings.Builder
	tmpl := template.Must(template.New("c").Option("missingkey=error").Parse(command))
	if err := tmpl.Execute(&cmd, map[string]string{"src_folder": "/work/src", "dst_folder": "/work/dst", "name": "clip", "extension": ext}); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "run", "--rm", "-v", configDir+":/cfg:ro", "-v", work+":/work",
		"-w", "/cfg/"+profile, "--entrypoint", "sh", image, "-c", cmd.String()).CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 3000 {
			tail = tail[len(tail)-3000:]
		}
		t.Fatalf("the %s task failed: %v\n%s", profile, err, tail)
	}
	files, _ := filepath.Glob(filepath.Join(work, "dst", "*"))
	if len(files) != 1 {
		t.Fatalf("the task left %d files, want 1", len(files))
	}
	return files[0]
}

// The transcoding profile on clips straight from phones: a portrait video
// from an iPhone and one in the Android layout, both with a position. After
// conversion Immich must still show the same date, place and shape.
func TestProfileKeepsVideoDatePlaceAndRotation(t *testing.T) {
	// It downloads about 90 MB and transcodes 4K video in software, which
	// takes minutes and every core it is given: only on request.
	if os.Getenv("E2E_VIDEO") == "" {
		t.Skip("set E2E_VIDEO=1 to run the video transcoding test")
	}
	big := &http.Client{Timeout: 20 * time.Minute}
	saved := httpc
	httpc = big
	defer func() { httpc = saved }()

	for _, clip := range []string{"train.mov", "waterfall.mp4"} {
		t.Run(clip, func(t *testing.T) {
			src := phoneClip(t, clip)
			converted := convertWith(t, "profile1", src)
			plain, through := newUser(t), newUser(t)
			status, out := plain.upload(t, immichURL, src, clip, nil)
			if status != http.StatusCreated {
				t.Fatalf("upload of the original: %d %s", status, out["_body"])
			}
			want := plain.videoFacts(t, out["id"])
			status, out = through.upload(t, immichURL, converted, "clip"+filepath.Ext(converted), nil)
			if status != http.StatusCreated {
				t.Fatalf("upload of the converted clip: %d %s", status, out["_body"])
			}
			got := through.videoFacts(t, out["id"])
			show := func(v videoFacts) string {
				return fmt.Sprintf("%vx%v, %s (local %s, zone %s), position %s %s, camera %s %s, orientation %s, %d bytes", v.Width, v.Height,
					v.Exif.DateTimeOriginal, v.LocalDateTime, ptr(v.Exif.TimeZone), ptr(v.Exif.Latitude), ptr(v.Exif.Longitude),
					ptr(v.Exif.Make), ptr(v.Exif.Model), ptr(v.Exif.Orientation), v.Exif.Size)
			}
			t.Logf("original:  %s", show(want))
			t.Logf("converted: %s", show(got))

			if got.Exif.DateTimeOriginal != want.Exif.DateTimeOriginal || got.LocalDateTime != want.LocalDateTime {
				t.Errorf("date: %s (local %s), want %s (local %s)", got.Exif.DateTimeOriginal, got.LocalDateTime, want.Exif.DateTimeOriginal, want.LocalDateTime)
			}
			if ptr(got.Exif.TimeZone) != ptr(want.Exif.TimeZone) {
				t.Errorf("time zone: %s, want %s", ptr(got.Exif.TimeZone), ptr(want.Exif.TimeZone))
			}
			if !near(got.Exif.Latitude, want.Exif.Latitude) || !near(got.Exif.Longitude, want.Exif.Longitude) {
				t.Errorf("position: %s, %s, want %s, %s", ptr(got.Exif.Latitude), ptr(got.Exif.Longitude), ptr(want.Exif.Latitude), ptr(want.Exif.Longitude))
			}
			// The shape as shown: upright stays upright.
			if (got.Width > got.Height) != (want.Width > want.Height) || got.Width == 0 {
				t.Errorf("shape: %vx%v, want the orientation of %vx%v", got.Width, got.Height, want.Width, want.Height)
			}
			if ptr(got.Exif.Make) != ptr(want.Exif.Make) || ptr(got.Exif.Model) != ptr(want.Exif.Model) {
				t.Errorf("camera: %s %s, want %s %s", ptr(got.Exif.Make), ptr(got.Exif.Model), ptr(want.Exif.Make), ptr(want.Exif.Model))
			}
		})
	}
}
