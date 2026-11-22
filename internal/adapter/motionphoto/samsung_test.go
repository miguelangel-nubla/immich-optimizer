package motionphoto

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// samsungTrailerBytes is a minimal Samsung trailer: "SEFH", an index, its
// own length and "SEFT".
func samsungTrailerBytes() []byte {
	b := []byte("SEFH\x6b\x00\x00\x00\x01\x00\x00\x00\x00\x00\x30\x0a\x24\x00\x00\x00\x24\x00\x00\x00")
	n := make([]byte, 4)
	binary.LittleEndian.PutUint32(n, uint32(len(b)))
	return append(append(b, n...), "SEFT"...)
}

// samsungJPEG builds a motion photo laid out as Samsung phones write one: the
// photo, "MotionPhoto_Data", the MP4 and Samsung's trailer. The video item's
// Length runs from the MP4 to the end of the file, trailer included; One UI 6
// gives the trailer's size as the item's Padding, One UI 5 does not.
func samsungJPEG(t *testing.T, declarePadding bool) (file, video []byte) {
	t.Helper()
	still, err := os.ReadFile(filepath.Join("testdata", "still.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	video, err = os.ReadFile(filepath.Join("testdata", "video.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	trailer := samsungTrailerBytes()
	padding := 0
	if declarePadding {
		padding = len(trailer)
	}
	xmp := fmt.Sprintf(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">`+
		`<rdf:Description xmlns:GCamera="http://ns.google.com/photos/1.0/camera/" xmlns:Container="http://ns.google.com/photos/1.0/container/" xmlns:Item="http://ns.google.com/photos/1.0/container/item/" GCamera:MotionPhoto="1" GCamera:MotionPhotoVersion="1">`+
		`<Container:Directory><rdf:Seq>`+
		`<rdf:li rdf:parseType="Resource"><Container:Item Item:Mime="image/jpeg" Item:Semantic="Primary" Item:Length="0" Item:Padding="16"/></rdf:li>`+
		`<rdf:li rdf:parseType="Resource"><Container:Item Item:Mime="video/mp4" Item:Semantic="MotionPhoto" Item:Length="%d" Item:Padding="%d"/></rdf:li>`+
		`</rdf:Seq></Container:Directory></rdf:Description></rdf:RDF></x:xmpmeta>`, len(video)+len(trailer), padding)
	seg := append([]byte(xmpHeader), xmp...)
	app1 := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(app1[2:], uint16(len(seg)+2))
	var b bytes.Buffer
	b.Write(still[:2]) // SOI
	b.Write(app1)
	b.Write(seg)
	b.Write(still[2:])
	b.WriteString("MotionPhoto_Data")
	b.Write(video)
	b.Write(trailer)
	return b.Bytes(), video
}

func TestSamsungMotionPhotoJPEG(t *testing.T) {
	for name, declared := range map[string]bool{"One UI 5 (no padding declared)": false, "One UI 6 (trailer as padding)": true} {
		t.Run(name, func(t *testing.T) {
			data, video := samsungJPEG(t, declared)
			dir := t.TempDir()
			src := filepath.Join(dir, "in.jpg")
			if err := os.WriteFile(src, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if !IsMotionPhoto(src) {
				t.Fatal("not recognised")
			}
			v := filepath.Join(dir, "v.mp4")
			if _, err := ExtractVideo(context.Background(), src, v); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(v); !bytes.Equal(got, video) {
				t.Errorf("extracted %d bytes, want exactly the %d of the MP4 (no trailer)", len(got), len(video))
			}
			s := filepath.Join(dir, "s.jpg")
			info, err := Strip(context.Background(), src, s)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(s)
			if int64(len(got)) != info.StillBytes || bytes.Contains(got, video[4:12]) || bytes.Contains(got, []byte("SEFT")) {
				t.Errorf("the photo (%d bytes) still holds the video or the trailer", len(got))
			}
			if IsMotionPhoto(s) {
				t.Error("the photo still claims a video")
			}
		})
	}
}

// Samsung HEICs index the video in a "sefd" box too. ExifTool keeps that box
// when it removes the video; it is turned into a free box, so that nothing
// points at a video that is gone.
func TestSamsungMotionPhotoHEIC(t *testing.T) {
	needExiftool(t)
	data, err := os.ReadFile(filepath.Join("testdata", "motion.heic"))
	if err != nil {
		t.Fatal(err)
	}
	index := append([]byte("\x00\x00\x00\x00"), samsungTrailerBytes()...)
	index = append(index, "MotionPhoto_Data mpv2"...)
	box := make([]byte, 8)
	binary.BigEndian.PutUint32(box, uint32(8+len(index)))
	copy(box[4:], "sefd")
	data = append(append(data, box...), index...)
	dir := t.TempDir()
	src := filepath.Join(dir, "in.heic")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.heic")
	if _, err := Strip(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(exiftool, "-s3", "-m", "-EmbeddedVideoType", "-MotionPhotoVideo", "-XMP-GCamera:MotionPhoto", dst).Output()
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(string(out)); s != "" {
		t.Errorf("the photo still announces a video: %q", s)
	}
	got, _ := os.ReadFile(dst)
	if bytes.Contains(got, []byte("sefd")) {
		t.Error("the Samsung index box is still there")
	}
}

// Real motion photos, when a folder of them is given: for example Immich's
// own (github.com/immich-app/test-assets, formats/motionphoto). Each one must
// be recognised and split, and its photo must not announce a video.
//
//	IO_MOTIONPHOTO_SAMPLES=/path/to/motionphoto go test -run RealSamples ./internal/adapter/motionphoto
func TestRealSamples(t *testing.T) {
	dir := os.Getenv("IO_MOTIONPHOTO_SAMPLES")
	if dir == "" {
		t.Skip("IO_MOTIONPHOTO_SAMPLES is not set")
	}
	needExiftool(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if e.IsDir() || (ext != ".jpg" && ext != ".jpeg" && ext != ".heic" && ext != ".heif" && ext != ".avif") {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			src := filepath.Join(dir, e.Name())
			if !IsMotionPhoto(src) {
				t.Fatal("not recognised as a motion photo")
			}
			out := t.TempDir()
			vi, err := ExtractVideo(context.Background(), src, filepath.Join(out, "v.mp4"))
			if err != nil {
				t.Fatal(err)
			}
			still := filepath.Join(out, "s"+ext)
			si, err := Strip(context.Background(), src, still)
			if err != nil {
				t.Fatal(err)
			}
			if vi.VideoBytes != si.VideoBytes {
				t.Errorf("video %d bytes when extracted, %d when stripped", vi.VideoBytes, si.VideoBytes)
			}
			if IsMotionPhoto(still) {
				t.Error("the photo still has its video")
			}
			tags, err := exec.Command(exiftool, "-s3", "-m", "-MotionPhotoVideo", "-EmbeddedVideoType", "-EmbeddedVideoFile", "-MicroVideo", still).Output()
			if err != nil {
				t.Fatal(err)
			}
			if s := strings.TrimSpace(string(tags)); s != "" {
				t.Errorf("the photo still announces a video: %q", s)
			}
			if mp, _ := exec.Command(exiftool, "-s3", "-m", "-XMP-GCamera:MotionPhoto", still).Output(); strings.TrimSpace(string(mp)) == "1" {
				t.Error("the photo still has MotionPhoto=1")
			}
			// An HDR photo (Pixel): its gain map can be removed as well.
			if HasGainMap(still) {
				sdr := filepath.Join(out, "sdr"+ext)
				gi, err := StripGainMap(context.Background(), still, sdr)
				if err != nil {
					t.Fatalf("removing the gain map: %v", err)
				}
				if HasGainMap(sdr) {
					t.Error("the gain map is still announced")
				}
				t.Logf("HDR photo: %d bytes without its gain map", gi.StillBytes)
			}
			t.Logf("photo %d bytes, video %d bytes", si.StillBytes, si.VideoBytes)
		})
	}
}
