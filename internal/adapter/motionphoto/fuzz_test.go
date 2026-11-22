package motionphoto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Any file may come in as a .jpg. The splitter must never crash on one, and
// whatever it splits must add up: the photo and the video are the whole
// file, the photo is its start (with the motion flags turned off, which
// keeps the length) and the video its end.
func FuzzSplit(f *testing.F) {
	for _, name := range []string{"motion.jpg", "microvideo.jpg", "still.jpg", "still_with_xmp_only.jpg"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		src := filepath.Join(dir, "in.jpg")
		if err := os.WriteFile(src, data, 0o600); err != nil {
			t.Fatal(err)
		}
		is := IsMotionJPEG(src)
		still, video := filepath.Join(dir, "still.jpg"), filepath.Join(dir, "video.mp4")
		si, serr := StripJPEG(src, still)
		vi, verr := ExtractVideoJPEG(src, video)
		if (serr == nil) != is || (verr == nil) != is {
			t.Fatalf("IsMotionJPEG %v, strip %v, extract %v", is, serr, verr)
		}
		if !is {
			for _, err := range []error{serr, verr} {
				if !errors.Is(err, ErrNotMotionPhoto) {
					t.Fatalf("a file that is not split must say ErrNotMotionPhoto: %v", err)
				}
			}
			for _, p := range []string{still, video} {
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("%s was written for a file that is not split", filepath.Base(p))
				}
			}
			return
		}
		if si.StillBytes+si.VideoBytes != int64(len(data)) || *si != *vi {
			t.Fatalf("the parts do not add up: %+v %+v of %d", si, vi, len(data))
		}
		s, _ := os.ReadFile(still)
		v, _ := os.ReadFile(video)
		if int64(len(s)) != si.StillBytes || !bytes.Equal(v, data[si.StillBytes:]) {
			t.Fatalf("photo %d bytes (want %d), video equal to the end: %v", len(s), si.StillBytes, bytes.Equal(v, data[si.StillBytes:]))
		}
		if IsMotionJPEG(still) {
			t.Fatal("the photo still claims a video")
		}
	})
}
