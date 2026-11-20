//go:build integration

// Integration tests for the bundled profiles. They run the real task commands
// (cjxl, caesiumclt, vips, magick, HandBrakeCLI, ffmpeg, exiftool) on the
// sample files in testdata/media, so they must run inside the Docker image:
// see scripts/integration-test.sh.
//
// The samples are synthetic: generated with ffmpeg, sips and exiftool. The
// Apple Live Photo identifier and the Samsung-style motion HEIC are built by
// hand to match what exiftool (and therefore Immich) reads.
package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/mediacheck"
	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/processor"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

const (
	liveID      = "11111111-2222-3333-4444-555555555555"
	sampleDate  = "2021:06:15 12:30:00"
	clipDateUTC = "2021:06:15 10:30:00"
)

func configRoot() string {
	if d := os.Getenv("IO_TEST_CONFIG_DIR"); d != "" {
		return d
	}
	return "../../config"
}

func mediaDir() string {
	if d := os.Getenv("IO_TEST_MEDIA_DIR"); d != "" {
		return d
	}
	return "testdata/media"
}

// runTool executes a tool and returns its trimmed stdout; a failure fails the test.
func runTool(t *testing.T, name string, args ...string) string {
	t.Helper()
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// tag returns one exiftool tag value ("" when absent).
func tag(t *testing.T, file, name string) string {
	t.Helper()
	return runTool(t, "exiftool", "-s3", "-m", "-"+name, file)
}

// runProfile runs the profile's tasks on a sample and returns the output file.
// override, when set, replaces the motion photo setting of every task.
func runProfile(t *testing.T, profile, sample, override string) *entity.ProcessResult {
	t.Helper()
	dir := filepath.Join(configRoot(), profile)
	cfg, err := loadTasks(filepath.Join(dir, "tasks.yaml"))
	if err != nil {
		t.Fatalf("loading %s: %v", profile, err)
	}
	if override != "" {
		for i := range cfg.Tasks {
			cfg.Tasks[i].MotionPhotos = override
		}
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := processor.NewLocalProcessor(nil, absDir).Process(context.Background(), filepath.Join(mediaDir(), sample), cfg.Tasks)
	if err != nil {
		t.Fatalf("%s on %s: %v", profile, sample, err)
	}
	t.Cleanup(res.Cleanup)
	return res
}

func sameBytes(t *testing.T, a, b string) bool {
	t.Helper()
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

// decodes fails the test unless the image can be fully decoded.
func decodes(t *testing.T, file string) {
	t.Helper()
	// vips reads JPEG XL (the image's ImageMagick does not).
	runTool(t, "vips", "copy", file, filepath.Join(t.TempDir(), "decoded.png"))
}

func ext(res *entity.ProcessResult) string {
	return strings.ToLower(filepath.Ext(res.ProcessedFilePath))
}

var convertingProfiles = []string{"lossless", "profile1", "storage-saver-amd-gpu"}

func TestIntegrationProfilesJPEG(t *testing.T) {
	src := filepath.Join(mediaDir(), "photo.jpg")
	for _, p := range convertingProfiles {
		t.Run(p, func(t *testing.T) {
			res := runProfile(t, p, "photo.jpg", "")
			if ext(res) != ".jxl" {
				t.Fatalf("output is %s, want .jxl", ext(res))
			}
			decodes(t, res.ProcessedFilePath)
			if got := tag(t, res.ProcessedFilePath, "ImageSize"); got != "640x480" {
				t.Errorf("ImageSize = %q, want 640x480", got)
			}
			if got := tag(t, res.ProcessedFilePath, "DateTimeOriginal"); got != sampleDate {
				t.Errorf("DateTimeOriginal = %q, want %q", got, sampleDate)
			}
			if tag(t, res.ProcessedFilePath, "GPSLatitude") == "" {
				t.Error("GPS position was lost")
			}
			if p == "lossless" {
				back := filepath.Join(t.TempDir(), "back.jpg")
				runTool(t, "djxl", res.ProcessedFilePath, back)
				if !sameBytes(t, back, src) {
					t.Error("lossless: the original JPEG cannot be rebuilt bit for bit from the JXL")
				}
			} else if res.ProcessedSize >= res.OriginalSize {
				t.Errorf("lossy output is not smaller: %d >= %d", res.ProcessedSize, res.OriginalSize)
			}
		})
	}
}

func TestIntegrationLosslessProfilePNGIsPixelIdentical(t *testing.T) {
	res := runProfile(t, "lossless", "photo.png", "")
	if ext(res) != ".jxl" {
		t.Fatalf("output is %s, want .jxl", ext(res))
	}
	decoded := filepath.Join(t.TempDir(), "decoded.png")
	runTool(t, "djxl", res.ProcessedFilePath, decoded)
	// AE is the number of differing pixels; compare exits 1 when they differ.
	cmd := exec.Command("magick", "compare", "-metric", "AE", filepath.Join(mediaDir(), "photo.png"), decoded, "null:")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(out)), "0") {
		t.Errorf("PNG is not pixel-identical after the lossless profile: %v %s", err, out)
	}
}

func TestIntegrationProfilesHEIC(t *testing.T) {
	src := filepath.Join(mediaDir(), "photo.heic")
	t.Run("lossless passes it through", func(t *testing.T) {
		res := runProfile(t, "lossless", "photo.heic", "")
		if !sameBytes(t, res.ProcessedFilePath, src) {
			t.Error("lossless profile changed a HEIC file")
		}
	})
	for _, p := range []string{"profile1", "storage-saver-amd-gpu"} {
		t.Run(p, func(t *testing.T) {
			res := runProfile(t, p, "photo.heic", "")
			if ext(res) != ".jxl" {
				t.Fatalf("output is %s, want .jxl", ext(res))
			}
			decodes(t, res.ProcessedFilePath)
			if got := tag(t, res.ProcessedFilePath, "ImageSize"); got != "640x480" {
				t.Errorf("ImageSize = %q, want 640x480", got)
			}
			if got := tag(t, res.ProcessedFilePath, "DateTimeOriginal"); got != sampleDate {
				t.Errorf("DateTimeOriginal = %q, want %q", got, sampleDate)
			}
			if tag(t, res.ProcessedFilePath, "GPSLatitude") == "" {
				t.Error("GPS position was lost")
			}
		})
	}
}

// Immich pairs the two files of an Apple Live Photo through the
// ContentIdentifier each of them carries. Both must survive conversion.
func TestIntegrationProfilesAppleLivePhoto(t *testing.T) {
	for _, p := range convertingProfiles {
		for _, still := range []string{"live.heic", "live.jpg"} {
			t.Run(p+"/"+still, func(t *testing.T) {
				res := runProfile(t, p, still, "")
				if got := tag(t, res.ProcessedFilePath, "ContentIdentifier"); got != liveID {
					t.Errorf("photo ContentIdentifier = %q, want %q: Immich would not pair it with its video", got, liveID)
				}
			})
		}
		t.Run(p+"/live.mov", func(t *testing.T) {
			if p == "storage-saver-amd-gpu" && !hasVAAPI() {
				t.Skip("no /dev/dri/renderD128: the GPU video task cannot run here")
			}
			res := runProfile(t, p, "live.mov", "")
			if got := tag(t, res.ProcessedFilePath, "ContentIdentifier"); got != liveID {
				t.Errorf("video ContentIdentifier = %q, want %q: Immich would not pair it with its photo", got, liveID)
			}
			runTool(t, "ffprobe", "-v", "error", "-show_entries", "format=duration", res.ProcessedFilePath)
		})
	}
}

func TestIntegrationProfilesMotionPhotoJPEG(t *testing.T) {
	for _, p := range convertingProfiles {
		t.Run(p, func(t *testing.T) {
			res := runProfile(t, p, "motion.jpg", "")
			decodes(t, res.ProcessedFilePath)
			if got := tag(t, res.ProcessedFilePath, "MotionPhoto"); got != "" && got != "0" {
				t.Errorf("output still claims to be a motion photo (MotionPhoto=%s)", got)
			}
			data, _ := os.ReadFile(res.ProcessedFilePath)
			if bytes.Contains(data, []byte("ftypisom")) || bytes.Contains(data, []byte("ftypmp4")) {
				t.Error("the embedded video is still inside the output")
			}
		})
	}
}

func TestIntegrationProfile1MotionPhotoHEIC(t *testing.T) {
	src := filepath.Join(mediaDir(), "motion.heic")
	t.Run("drop_video (bundled default)", func(t *testing.T) {
		res := runProfile(t, "profile1", "motion.heic", "")
		if ext(res) != ".jxl" {
			t.Fatalf("output is %s, want .jxl", ext(res))
		}
		decodes(t, res.ProcessedFilePath)
		if got := tag(t, res.ProcessedFilePath, "MotionPhoto"); got != "" && got != "0" {
			t.Errorf("output still claims to be a motion photo (MotionPhoto=%s)", got)
		}
		if got := tag(t, res.ProcessedFilePath, "DateTimeOriginal"); got != sampleDate {
			t.Errorf("DateTimeOriginal = %q, want %q", got, sampleDate)
		}
	})
	t.Run("keep", func(t *testing.T) {
		res := runProfile(t, "profile1", "motion.heic", entity.MotionPhotosKeep)
		if !sameBytes(t, res.ProcessedFilePath, src) {
			t.Error("keep must pass the HEIC motion photo through unchanged")
		}
	})
	t.Run("drop_photo", func(t *testing.T) {
		res := runProfile(t, "profile1", "motion.heic", entity.MotionPhotosDropPhoto)
		if ext(res) != ".mp4" {
			t.Fatalf("output is %s, want .mp4", ext(res))
		}
		if !sameBytes(t, res.ProcessedFilePath, filepath.Join(mediaDir(), "live.mov")) {
			t.Error("drop_photo must output exactly the embedded video")
		}
	})
}

// The GPU profile converts HEIC with ImageMagick, which only reads the still:
// the output must not claim to be a motion photo either.
func TestIntegrationProfileGPUMotionPhotoHEIC(t *testing.T) {
	res := runProfile(t, "storage-saver-amd-gpu", "motion.heic", "")
	decodes(t, res.ProcessedFilePath)
	if got := tag(t, res.ProcessedFilePath, "MotionPhoto"); got != "" && got != "0" {
		t.Errorf("output still claims to be a motion photo (MotionPhoto=%s)", got)
	}
	if got := tag(t, res.ProcessedFilePath, "DateTimeOriginal"); got != sampleDate {
		t.Errorf("DateTimeOriginal = %q, want %q", got, sampleDate)
	}
}

func hasVAAPI() bool {
	_, err := os.Stat("/dev/dri/renderD128")
	return err == nil
}

// displaySize returns the size a player shows, after applying rotation.
func displaySize(t *testing.T, file string) (w, h int) {
	t.Helper()
	out := runTool(t, "ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height:stream_side_data=rotation", "-of", "default=nw=1", file)
	rotation := 0
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		n, _ := strconv.Atoi(v)
		switch k {
		case "width":
			w = n
		case "height":
			h = n
		case "rotation":
			rotation = n
		}
	}
	if rotation%180 != 0 {
		w, h = h, w
	}
	return w, h
}

// clip.mp4 is a 640x360 recording with a 90 degree rotation flag: a phone
// video shot upright. It must still be upright after conversion.
func TestIntegrationProfilesVideo(t *testing.T) {
	src := filepath.Join(mediaDir(), "clip.mp4")
	sw, sh := displaySize(t, src)
	if sw >= sh {
		t.Fatalf("fixture is not portrait: %dx%d", sw, sh)
	}
	t.Run("lossless passes it through", func(t *testing.T) {
		res := runProfile(t, "lossless", "clip.mp4", "")
		if !sameBytes(t, res.ProcessedFilePath, src) {
			t.Error("lossless profile changed a video")
		}
	})
	for _, p := range []string{"profile1", "storage-saver-amd-gpu"} {
		t.Run(p, func(t *testing.T) {
			if p == "storage-saver-amd-gpu" && !hasVAAPI() {
				t.Skip("no /dev/dri/renderD128: the GPU video task cannot run here")
			}
			res := runProfile(t, p, "clip.mp4", "")
			if res.ProcessedSize >= res.OriginalSize {
				t.Errorf("video output is not smaller: %d >= %d", res.ProcessedSize, res.OriginalSize)
			}
			w, h := displaySize(t, res.ProcessedFilePath)
			if w >= h {
				t.Errorf("portrait video became landscape or sideways: displays as %dx%d (source %dx%d)", w, h, sw, sh)
			}
			if got := tag(t, res.ProcessedFilePath, "CreateDate"); got != clipDateUTC {
				t.Errorf("CreateDate = %q, want %q", got, clipDateUTC)
			}
			if tag(t, res.ProcessedFilePath, "GPSCoordinates") == "" {
				t.Error("GPS position was lost")
			}
			if runTool(t, "ffprobe", "-v", "error", "-select_streams", "a", "-show_entries", "stream=codec_name", "-of", "csv=p=0", res.ProcessedFilePath) == "" {
				t.Error("audio track was lost")
			}
		})
	}
}

func TestIntegrationPassthroughProfileChangesNothing(t *testing.T) {
	for _, sample := range []string{"photo.jpg", "photo.heic", "photo.png", "motion.jpg", "motion.heic", "live.mov", "clip.mp4"} {
		res := runProfile(t, "passthrough-all", sample, "")
		if !sameBytes(t, res.ProcessedFilePath, filepath.Join(mediaDir(), sample)) {
			t.Errorf("passthrough-all changed %s", sample)
		}
	}
}

// The validity check library mode applies to converted files, run with the
// image's real tools.
func TestIntegrationOutputCheck(t *testing.T) {
	ctx := context.Background()
	c := mediacheck.New()
	media := func(name string) string { return filepath.Join(mediaDir(), name) }

	// Real conversions pass, lossy ones included.
	for _, p := range convertingProfiles {
		for _, sample := range []string{"photo.jpg", "photo.png", "photo.heic"} {
			res := runProfile(t, p, sample, "")
			if err := c.Check(ctx, media(sample), res.ProcessedFilePath); err != nil {
				t.Errorf("%s on %s: %v", p, sample, err)
			}
		}
	}
	res := runProfile(t, "profile1", "clip.mp4", "")
	if err := c.Check(ctx, media("clip.mp4"), res.ProcessedFilePath); err != nil {
		t.Errorf("profile1 video: %v", err)
	}

	// A different picture is fine: tasks may crop or watermark.
	if err := c.Check(ctx, media("photo.jpg"), media("faces/face1.jpg")); err != nil {
		t.Errorf("a different valid photo was rejected: %v", err)
	}

	dir := t.TempDir()
	bad := func(name string, make func(path string)) {
		path := filepath.Join(dir, name)
		make(path)
		if err := c.Check(ctx, media("photo.jpg"), path); err == nil {
			t.Errorf("%s was accepted", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
	bad("flat.png", func(p string) { runTool(t, "magick", "-size", "640x480", "xc:gray", p) })
	bad("black.jxl", func(p string) {
		ppm := filepath.Join(dir, "black.ppm")
		runTool(t, "magick", "-size", "640x480", "xc:black", ppm)
		runTool(t, "cjxl", ppm, p, "-d", "0", "--quiet")
	})
	bad("garbage.jxl", func(p string) { os.WriteFile(p, bytes.Repeat([]byte("not an image "), 200), 0o600) })
	bad("truncated.jpg", func(p string) {
		data, _ := os.ReadFile(media("photo.jpg"))
		os.WriteFile(p, data[:len(data)/20], 0o600)
	})
	bad("empty.jxl", func(p string) { os.WriteFile(p, nil, 0o600) })
	bad("audio-only.mp4", func(p string) {
		runTool(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "sine=duration=1", "-c:a", "aac", p)
	})
	bad("black.mp4", func(p string) {
		runTool(t, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=black:size=320x240:rate=10:duration=1", "-pix_fmt", "yuv420p", p)
	})

	// A flat original may stay flat.
	flat := filepath.Join(dir, "flat-original.png")
	runTool(t, "magick", "-size", "64x64", "xc:white", flat)
	if err := c.Check(ctx, flat, flat); err != nil {
		t.Errorf("a flat copy of a flat original was rejected: %v", err)
	}
}
