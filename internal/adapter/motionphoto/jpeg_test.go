package motionphoto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures were written with exiftool: motion.jpg uses the Google
// MotionPhoto + Container directory form, microvideo.jpg the older MicroVideo
// form. Both end with video.mp4.

func TestStripJPEG(t *testing.T) {
	video, err := os.ReadFile("testdata/video.mp4")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"motion.jpg", "microvideo.jpg"} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join("testdata", name)
			orig, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(t.TempDir(), "out.jpg")
			info, err := StripJPEG(src, dst)
			if err != nil {
				t.Fatalf("StripJPEG: %v", err)
			}
			if info.VideoBytes != int64(len(video)) {
				t.Errorf("VideoBytes = %d, want %d", info.VideoBytes, len(video))
			}
			out, err := os.ReadFile(dst)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != len(orig)-len(video) || info.StillBytes != int64(len(out)) {
				t.Fatalf("still is %d bytes, want %d", len(out), len(orig)-len(video))
			}
			if bytes.Contains(out, []byte("ftyp")) {
				t.Error("video is still in the output")
			}
			// Only the flag bytes may differ from the original still part.
			diff := 0
			for i := range out {
				if out[i] != orig[i] {
					diff++
					if orig[i] != '1' || out[i] != '0' {
						t.Fatalf("unexpected change at byte %d: %q -> %q", i, orig[i], out[i])
					}
				}
			}
			if diff != 1 {
				t.Errorf("%d bytes changed, want exactly the one motion flag", diff)
			}
			// The result is no longer a motion photo.
			if _, err := StripJPEG(dst, filepath.Join(t.TempDir(), "again.jpg")); !errors.Is(err, ErrNotMotionPhoto) {
				t.Errorf("stripping the result again: err = %v, want ErrNotMotionPhoto", err)
			}
			// The input is untouched.
			after, _ := os.ReadFile(src)
			if !bytes.Equal(after, orig) {
				t.Error("input file was modified")
			}
		})
	}
}

func TestStripJPEGNotMotionPhoto(t *testing.T) {
	dir := t.TempDir()
	notJPEG := filepath.Join(dir, "x.jpg")
	if err := os.WriteFile(notJPEG, []byte("not a jpeg at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Flags set but no video at the end: must be left alone.
	cases := []string{"testdata/still.jpg", "testdata/still_with_xmp_only.jpg", "testdata/video.mp4", notJPEG}
	for _, src := range cases {
		dst := filepath.Join(dir, "out-"+filepath.Base(src))
		if _, err := StripJPEG(src, dst); !errors.Is(err, ErrNotMotionPhoto) {
			t.Errorf("%s: err = %v, want ErrNotMotionPhoto", src, err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("%s: an output file was written", src)
		}
	}
}

func TestStripJPEGElementForm(t *testing.T) {
	// XMP written with elements instead of attributes.
	xmp := `<x:xmpmeta><rdf:Description><GCamera:MicroVideo>1</GCamera:MicroVideo>` +
		`<GCamera:MicroVideoOffset>12</GCamera:MicroVideoOffset></rdf:Description></x:xmpmeta>`
	payload := append([]byte(xmpHeader), xmp...)
	var b bytes.Buffer
	b.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)})
	b.Write(payload)
	b.Write([]byte{0xFF, 0xDA, 0x00, 0x02, 0xFF, 0xD9})
	b.WriteString("\x00\x00\x00\x0cftypmp42") // 12-byte "video"
	src := filepath.Join(t.TempDir(), "e.jpg")
	if err := os.WriteFile(src, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "o.jpg")
	info, err := StripJPEG(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(dst)
	if info.VideoBytes != 12 || !bytes.Contains(out, []byte("<GCamera:MicroVideo>0</GCamera:MicroVideo>")) {
		t.Errorf("info=%+v out=%q", info, out)
	}
}
