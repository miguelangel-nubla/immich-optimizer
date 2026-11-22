package motionphoto

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Any file may come in as a .jpg. The splitter must never crash on one, and
// whatever it splits must add up: the photo is the start of the file (with
// the motion flags turned off, which keeps the length) and the video what
// follows it, up to the end or to a trailer.
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
		_ = HasGainMap(src) // must not crash on anything
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
		// What follows the video (a Samsung trailer) is neither: at most a
		// few bytes short of the end.
		if si.StillBytes+si.VideoBytes > int64(len(data)) || si.VideoBytes < 8 || *si != *vi {
			t.Fatalf("the parts do not add up: %+v %+v of %d", si, vi, len(data))
		}
		s, _ := os.ReadFile(still)
		v, _ := os.ReadFile(video)
		if int64(len(s)) != si.StillBytes || !bytes.Equal(v, data[si.StillBytes:si.StillBytes+si.VideoBytes]) {
			t.Fatalf("photo %d bytes (want %d), video is the part after it: %v", len(s), si.StillBytes, bytes.Equal(v, data[si.StillBytes:si.StillBytes+si.VideoBytes]))
		}
		if IsMotionJPEG(still) {
			t.Fatal("the photo still claims a video")
		}
	})
}

// Any file may come in as a .heic. Finding its video must never crash, and
// what is found must be the end of an mpvd box that holds an MP4: the copy
// starts with an ftyp box and is part of the file.
func FuzzSplitHEIF(f *testing.F) {
	for _, name := range []string{"motion.heic", "still.heic", "video.mp4"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		src := filepath.Join(dir, "in.heic")
		if err := os.WriteFile(src, data, 0o600); err != nil {
			t.Fatal(err)
		}
		video := filepath.Join(dir, "video.mp4")
		info, err := ExtractVideoHEIF(src, video)
		if err != nil {
			if !errors.Is(err, ErrNotMotionPhoto) {
				t.Fatalf("a file that is not split must say ErrNotMotionPhoto: %v", err)
			}
			if _, serr := os.Stat(video); serr == nil {
				t.Fatal("something was written for a file that was not split")
			}
			return
		}
		got, err := os.ReadFile(video)
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(got)) != info.VideoBytes || len(got) < 8 || string(got[4:8]) != "ftyp" || !bytes.Contains(data, got) {
			t.Fatalf("extracted %d bytes (info %+v) that are not an MP4 from the file", len(got), info)
		}
		if info.StillBytes+info.VideoBytes > int64(len(data)) {
			t.Fatalf("photo %d + video %d bytes is more than the %d of the file", info.StillBytes, info.VideoBytes, len(data))
		}
	})
}
