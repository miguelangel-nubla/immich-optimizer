package motionphoto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mpvdPayload returns the video inside the sample's mpvd box, found the
// simple way: by scanning for the box type.
func mpvdPayload(t *testing.T, data []byte) []byte {
	t.Helper()
	i := bytes.Index(data, []byte("mpvd"))
	if i < 4 {
		t.Fatal("the sample has no mpvd box")
	}
	size := int(binary.BigEndian.Uint32(data[i-4 : i]))
	return data[i+4 : i-4+size]
}

func TestIsMotionPhotoHEIF(t *testing.T) {
	for name, want := range map[string]bool{"motion.heic": true, "still.heic": false, "video.mp4": false, "motion.jpg": true, "still.jpg": false} {
		if got := IsMotionPhoto(filepath.Join("testdata", name)); got != want {
			t.Errorf("IsMotionPhoto(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestExtractVideoHEIF(t *testing.T) {
	src := filepath.Join("testdata", "motion.heic")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "video.mp4")
	info, err := ExtractVideo(context.Background(), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	want := mpvdPayload(t, data)
	if !bytes.Equal(got, want) || info.VideoBytes != int64(len(want)) {
		t.Errorf("extracted %d bytes (info %d), want the %d of the mpvd box", len(got), info.VideoBytes, len(want))
	}
	if string(got[4:8]) != "ftyp" {
		t.Errorf("the video does not start with an ftyp box")
	}
	if _, err := ExtractVideo(context.Background(), filepath.Join("testdata", "still.heic"), filepath.Join(t.TempDir(), "x.mp4")); !errors.Is(err, ErrNotMotionPhoto) {
		t.Errorf("a still HEIC: %v", err)
	}
}

func needExiftool(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(exiftool); err != nil {
		t.Skip("exiftool is not installed")
	}
}

func TestStripHEIF(t *testing.T) {
	needExiftool(t)
	src := filepath.Join(t.TempDir(), "motion.heic")
	data, _ := os.ReadFile(filepath.Join("testdata", "motion.heic"))
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mod := time.Date(2021, 6, 15, 12, 30, 0, 0, time.UTC)
	if err := os.Chtimes(src, mod, mod); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "still.heic")
	info, err := Strip(context.Background(), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.StillBytes != st.Size() || st.Size() >= int64(len(data)) || info.VideoBytes != int64(len(mpvdPayload(t, data))) {
		t.Errorf("info %+v for a %d byte result of a %d byte file", info, st.Size(), len(data))
	}
	if !st.ModTime().Equal(mod) {
		t.Errorf("modification time %v, want the original's %v", st.ModTime(), mod)
	}
	if IsMotionPhoto(dst) {
		t.Error("the result still has its video")
	}
	out, err := exec.Command(exiftool, "-s", "-s", "-s", "-XMP-GCamera:MotionPhoto", "-MotionPhotoVideo", "-DateTimeOriginal", dst).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "2021:06:15 12:30:00" {
		t.Errorf("exiftool reads %q from the result: the motion metadata must be gone and the date kept", got)
	}
	// The input is left as it was.
	if again, _ := os.ReadFile(src); !bytes.Equal(again, data) {
		t.Error("the input was modified")
	}
	// A still is not split.
	if _, err := Strip(context.Background(), filepath.Join("testdata", "still.heic"), filepath.Join(t.TempDir(), "x.heic")); !errors.Is(err, ErrNotMotionPhoto) {
		t.Errorf("a still HEIC: %v", err)
	}
}

// Without ExifTool the video cannot be removed: that is an error (the task
// fails and the file is kept), never a photo that still has its video.
func TestStripHEIFWithoutExiftool(t *testing.T) {
	old := exiftool
	exiftool = filepath.Join(t.TempDir(), "no-such-exiftool")
	defer func() { exiftool = old }()
	dst := filepath.Join(t.TempDir(), "still.heic")
	_, err := Strip(context.Background(), filepath.Join("testdata", "motion.heic"), dst)
	if err == nil || errors.Is(err, ErrNotMotionPhoto) || !strings.Contains(err.Error(), "ExifTool") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("a result was left behind")
	}
}

// A box that claims more than the file holds, a zero-size box in the middle,
// an mpvd box with no MP4 in it: not split, and no panic.
func TestAnalyzeHEIFBrokenBoxes(t *testing.T) {
	box := func(typ string, payload []byte) []byte {
		b := make([]byte, 8, 8+len(payload))
		binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
		copy(b[4:], typ)
		return append(b, payload...)
	}
	ftyp := box("ftyp", []byte("heic\x00\x00\x00\x00mif1heic"))
	mp4 := box("ftyp", []byte("isom\x00\x00\x00\x00isom"))
	cases := map[string][]byte{
		"too long":      append(append([]byte(nil), ftyp...), 0, 0, 1, 0, 'm', 'p', 'v', 'd'),
		"no mp4":        append(append([]byte(nil), ftyp...), box("mpvd", []byte("not a video at all"))...),
		"tiny":          append(append([]byte(nil), ftyp...), box("mpvd", nil)...),
		"not first":     append(box("free", nil), box("mpvd", mp4)...),
		"size under 8":  append(append([]byte(nil), ftyp...), 0, 0, 0, 3, 'm', 'p', 'v', 'd'),
		"huge 64-bit":   append(append([]byte(nil), ftyp...), 0, 0, 0, 1, 'm', 'p', 'v', 'd', 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF),
		"truncated hdr": append(append([]byte(nil), ftyp...), 0, 0),
	}
	for name, data := range cases {
		p := filepath.Join(t.TempDir(), "x.heic")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if IsMotionPhoto(p) {
			t.Errorf("%s: taken for a motion photo", name)
		}
	}
	// And the well-formed one is.
	p := filepath.Join(t.TempDir(), "ok.heic")
	if err := os.WriteFile(p, append(append([]byte(nil), ftyp...), box("mpvd", mp4)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if !IsMotionPhoto(p) {
		t.Error("a minimal HEIF motion photo was not recognised")
	}
}
