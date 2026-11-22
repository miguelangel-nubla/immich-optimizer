package motionphoto

import (
	"context"
	"errors"
	"os"
)

// kind is the container a file is in, from its first bytes.
type kind int

const (
	kindOther kind = iota
	kindJPEG
	kindHEIF
)

func sniff(path string) (kind, error) {
	f, err := os.Open(path)
	if err != nil {
		return kindOther, err
	}
	defer f.Close()
	head := make([]byte, 12)
	n, err := f.Read(head)
	if err := readErr(err); err != nil {
		return kindOther, err
	}
	head = head[:n]
	switch {
	case len(head) >= 2 && head[0] == 0xFF && head[1] == 0xD8:
		return kindJPEG, nil
	case isHEIF(head):
		return kindHEIF, nil
	}
	return kindOther, nil
}

// IsMotionPhoto reports whether the file is a motion photo this package can
// split: a JPEG or a HEIF (HEIC, AVIF) one.
//
// A file that cannot be read counts as one: whoever asks then leaves it as
// it is rather than risk hiding a video.
func IsMotionPhoto(path string) bool {
	k, err := sniff(path)
	if err != nil {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	switch k {
	case kindJPEG:
		_, err = analyze(f)
	case kindHEIF:
		_, err = analyzeHEIF(f)
	default:
		return false
	}
	return !errors.Is(err, ErrNotMotionPhoto)
}

// Strip writes the photo of the motion photo at src to dst, without the video
// and with the metadata that announces one switched off. It returns
// ErrNotMotionPhoto (and writes nothing) when src is not a motion photo this
// package can split.
func Strip(ctx context.Context, src, dst string) (*Info, error) {
	k, err := sniff(src)
	if err != nil {
		return nil, err
	}
	switch k {
	case kindJPEG:
		return StripJPEG(src, dst)
	case kindHEIF:
		return StripHEIF(ctx, src, dst)
	}
	return nil, ErrNotMotionPhoto
}

// ExtractVideo writes the video of the motion photo at src to dst. It returns
// ErrNotMotionPhoto (and writes nothing) when src is not a motion photo this
// package can split.
func ExtractVideo(_ context.Context, src, dst string) (*Info, error) {
	k, err := sniff(src)
	if err != nil {
		return nil, err
	}
	switch k {
	case kindJPEG:
		return ExtractVideoJPEG(src, dst)
	case kindHEIF:
		return ExtractVideoHEIF(src, dst)
	}
	return nil, ErrNotMotionPhoto
}
