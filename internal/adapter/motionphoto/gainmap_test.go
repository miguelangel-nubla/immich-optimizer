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
)

// gainmap.jpg is still.jpg announcing an Ultra HDR gain map in its XMP, with a
// second JPEG (the gain map) after it.
func TestHasGainMap(t *testing.T) {
	for name, want := range map[string]bool{"gainmap.jpg": true, "still.jpg": false, "motion.jpg": false, "motion.heic": false, "video.mp4": false} {
		if got := HasGainMap(filepath.Join("testdata", name)); got != want {
			t.Errorf("HasGainMap(%s) = %v, want %v", name, got, want)
		}
	}
	// The ISO 21496-1 form: an APP2 segment in the photo, no XMP.
	still, _ := os.ReadFile(filepath.Join("testdata", "still.jpg"))
	seg := []byte(isoGainMapID + "\x00\x00")
	app2 := []byte{0xFF, 0xE2, 0, 0}
	binary.BigEndian.PutUint16(app2[2:], uint16(len(seg)+2))
	iso := append(append(append(append([]byte(nil), still[:2]...), app2...), seg...), still[2:]...)
	p := filepath.Join(t.TempDir(), "iso.jpg")
	if err := os.WriteFile(p, iso, 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasGainMap(p) {
		t.Error("an ISO 21496-1 gain map photo was not recognised")
	}
	if !HasGainMap(t.TempDir()) {
		t.Error("an unreadable file counted as an ordinary photo")
	}
}

func TestStripGainMap(t *testing.T) {
	needExiftool(t)
	src := filepath.Join("testdata", "gainmap.jpg")
	dst := filepath.Join(t.TempDir(), "sdr.jpg")
	info, err := StripGainMap(context.Background(), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	still, _ := os.ReadFile(filepath.Join("testdata", "still.jpg"))
	if int64(len(got)) != info.StillBytes || HasGainMap(dst) {
		t.Errorf("result %d bytes (info %d), gain map still announced: %v", len(got), info.StillBytes, HasGainMap(dst))
	}
	if bytes.Count(got, still[len(still)-64:]) > 1 {
		t.Error("the gain map image is still in the file")
	}
	out, _ := exec.Command(exiftool, "-s3", "-m", "-XMP-hdrgm:Version", "-MPImageType", dst).Output()
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("the photo still announces a gain map: %q", s)
	}
	if _, err := StripGainMap(context.Background(), filepath.Join("testdata", "still.jpg"), filepath.Join(t.TempDir(), "x.jpg")); !errors.Is(err, ErrNoGainMap) {
		t.Errorf("an ordinary photo: %v", err)
	}
}

// A motion photo that still has its video keeps its gain map: removing what
// follows the photo would take the video with it.
func TestStripGainMapRefusesAMotionPhotoWithItsVideo(t *testing.T) {
	needExiftool(t)
	data, _ := samsungJPEG(t, true)
	// Announce a gain map too.
	data = bytes.Replace(data, []byte(`GCamera:MotionPhoto="1"`), []byte(`GCamera:MotionPhoto="1" hdrgm:Version="1.0"`), 1)
	data = fixXMPLength(t, data)
	src := filepath.Join(t.TempDir(), "both.jpg")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if !HasGainMap(src) || !IsMotionPhoto(src) {
		t.Fatal("fixture not recognised")
	}
	dst := filepath.Join(t.TempDir(), "out.jpg")
	if _, err := StripGainMap(context.Background(), src, dst); err == nil || errors.Is(err, ErrNoGainMap) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a result was written")
	}
}

// fixXMPLength corrects the length of the first APP1 segment after its
// content was changed.
func fixXMPLength(t *testing.T, data []byte) []byte {
	t.Helper()
	if data[2] != 0xFF || data[3] != 0xE1 {
		t.Fatal("no APP1 first")
	}
	end := bytes.Index(data[4:], []byte("</x:xmpmeta>")) + 4 + len("</x:xmpmeta>")
	binary.BigEndian.PutUint16(data[4:6], uint16(end-4))
	return data
}
