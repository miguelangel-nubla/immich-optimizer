package processor

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// The fixture is a JPEG motion photo that ends with video.mp4.
const motionFixtures = "../motionphoto/testdata"

func TestMotionPhotosPerTask(t *testing.T) {
	orig, err := os.ReadFile(filepath.Join(motionFixtures, "motion.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	video, err := os.ReadFile(filepath.Join(motionFixtures, "video.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(t.TempDir(), "IMG_1.jpg")
	if err := os.WriteFile(in, orig, 0o600); err != nil {
		t.Fatal(err)
	}

	// The command copies whatever it receives and records the setting it saw.
	const command = `cp "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/" && test "$IO_MOTION_PHOTOS" = "$EXPECT"`
	cases := []struct {
		setting, wantName string
		wantSize          int
	}{
		{entity.MotionPhotosKeep, "IMG_1.jpg", len(orig)},
		{entity.MotionPhotosDropVideo, "IMG_1.jpg", len(orig) - len(video)},
		{entity.MotionPhotosDropPhoto, "IMG_1.mp4", len(video)},
	}
	for _, c := range cases {
		t.Run(c.setting, func(t *testing.T) {
			t.Setenv("EXPECT", c.setting)
			p := NewLocalProcessor(nil, t.TempDir())
			res, err := p.Process(context.Background(), in, []entity.Task{
				{Name: "t", Extensions: []string{"jpg"}, Command: command, MotionPhotos: c.setting},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer res.Cleanup()
			out, err := os.ReadFile(res.ProcessedFilePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != c.wantSize || res.ProcessedFilename != c.wantName {
				t.Errorf("task received %d bytes as %q, want %d as %q", len(out), res.ProcessedFilename, c.wantSize, c.wantName)
			}
			if c.setting == entity.MotionPhotosDropPhoto && !bytes.Equal(out, video) {
				t.Error("drop_photo did not pass the exact embedded video")
			}
			if c.setting == entity.MotionPhotosDropVideo && bytes.Contains(out, []byte("ftyp")) {
				t.Error("drop_video still contains the video")
			}
			// Sizes are reported against the real input.
			if res.OriginalSize != int64(len(orig)) {
				t.Errorf("OriginalSize = %d, want %d", res.OriginalSize, len(orig))
			}
		})
	}
}

func TestMotionPhotosFallbackTasksEachGetTheirOwnInput(t *testing.T) {
	orig, _ := os.ReadFile(filepath.Join(motionFixtures, "motion.jpg"))
	video, _ := os.ReadFile(filepath.Join(motionFixtures, "video.mp4"))
	in := filepath.Join(t.TempDir(), "IMG_1.jpg")
	if err := os.WriteFile(in, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := NewLocalProcessor(nil, t.TempDir()).Process(context.Background(), in, []entity.Task{
		{Name: "fails", Extensions: []string{"jpg"}, Command: "exit 1", MotionPhotos: entity.MotionPhotosDropVideo},
		{Name: "passthrough", Extensions: []string{"jpg"}, Command: "", MotionPhotos: entity.MotionPhotosDropPhoto},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	out, _ := os.ReadFile(res.ProcessedFilePath)
	if !bytes.Equal(out, video) || res.ProcessedFilename != "IMG_1.mp4" {
		t.Errorf("fallback task got %d bytes as %q, want the video as IMG_1.mp4", len(out), res.ProcessedFilename)
	}
}

func TestMotionPhotosSettingIgnoredForOrdinaryFiles(t *testing.T) {
	still, _ := os.ReadFile(filepath.Join(motionFixtures, "still.jpg"))
	in := filepath.Join(t.TempDir(), "plain.jpg")
	if err := os.WriteFile(in, still, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{entity.MotionPhotosDropVideo, entity.MotionPhotosDropPhoto} {
		res, err := NewLocalProcessor(nil, t.TempDir()).Process(context.Background(), in, []entity.Task{
			{Name: "t", Extensions: []string{"jpg"}, Command: "", MotionPhotos: setting},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, _ := os.ReadFile(res.ProcessedFilePath)
		if !bytes.Equal(out, still) {
			t.Errorf("%s: an ordinary JPEG was changed", setting)
		}
		res.Cleanup()
	}
}

// HEIC motion photos are split like JPEG ones: the task gets the photo
// without the video, or only the video.
func TestMotionPhotosHEIC(t *testing.T) {
	orig, err := os.ReadFile(filepath.Join(motionFixtures, "motion.heic"))
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(t.TempDir(), "IMG_2.heic")
	if err := os.WriteFile(in, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(setting string) (*entity.ProcessResult, []byte) {
		t.Helper()
		res, err := NewLocalProcessor(nil, t.TempDir()).Process(context.Background(), in, []entity.Task{
			{Name: "t", Extensions: []string{"heic"}, Command: "", MotionPhotos: setting},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(res.Cleanup)
		out, err := os.ReadFile(res.ProcessedFilePath)
		if err != nil {
			t.Fatal(err)
		}
		return res, out
	}

	res, out := run(entity.MotionPhotosDropPhoto)
	if res.ProcessedFilename != "IMG_2.mp4" || len(out) < 8 || string(out[4:8]) != "ftyp" || res.MotionPhotos != entity.MotionPhotosDropPhoto {
		t.Errorf("drop_photo: got %d bytes as %q (split %q)", len(out), res.ProcessedFilename, res.MotionPhotos)
	}
	if !bytes.Contains(orig, out) {
		t.Error("drop_photo did not pass the exact embedded video")
	}

	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool is not installed: drop_video is not tried")
	}
	res, out = run(entity.MotionPhotosDropVideo)
	if res.ProcessedFilename != "IMG_2.heic" || len(out) >= len(orig) || bytes.Contains(out, []byte("mpvd")) || res.MotionPhotos != entity.MotionPhotosDropVideo {
		t.Errorf("drop_video: got %d of %d bytes as %q (split %q)", len(out), len(orig), res.ProcessedFilename, res.MotionPhotos)
	}
}
