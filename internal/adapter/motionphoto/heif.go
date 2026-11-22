package motionphoto

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

// HEIF motion photos (HEIC, and AVIF which is the same container): the
// Motion Photo format puts the video in a top-level "mpvd" box of the file,
// next to the boxes of the photo.
//
// Finding the video and copying it out is done here. Removing it is not: the
// photo's data is located by absolute file offsets (the "iloc" box), which
// all move when a box before them goes. ExifTool rewrites HEIF files and
// their offsets; it removes the video and the motion metadata, and the result
// is checked before it is used.

// exiftool is the ExifTool command.
var exiftool = "exiftool"

// maxBoxes bounds how many top-level boxes are looked at: a HEIF file has a
// handful.
const maxBoxes = 4096

// heifLayout is what analyzeHEIF learns about a HEIF motion photo.
type heifLayout struct {
	size     int64
	box      int64 // the mpvd box's size, header included
	videoOff int64
	videoLen int64
	modTime  time.Time
}

// isHEIF reports whether the first bytes of a file are an ISO-BMFF "ftyp"
// box: HEIC, HEIF, AVIF (and MP4 or MOV, which are told apart by the file
// name before this is asked).
func isHEIF(head []byte) bool {
	return len(head) >= 8 && string(head[4:8]) == "ftyp"
}

// walkBoxes calls fn for each top-level box of f: its offset, size (header
// included), header length and type, until fn returns true. A file that does
// not start with an ftyp box, or has a broken box before fn stops, is
// ErrNotMotionPhoto.
func walkBoxes(f *os.File, size int64, fn func(off, boxSize, headLen int64, typ string) (stop bool, err error)) error {
	hdr := make([]byte, 16)
	var off int64
	for n := 0; off+8 <= size; n++ {
		if n >= maxBoxes {
			return ErrNotMotionPhoto
		}
		if _, err := f.ReadAt(hdr[:8], off); err != nil {
			return notSplit(err)
		}
		boxSize, typ, headLen := int64(binary.BigEndian.Uint32(hdr[:4])), string(hdr[4:8]), int64(8)
		switch boxSize {
		case 0: // to the end of the file
			boxSize = size - off
		case 1: // a 64-bit size follows
			if _, err := f.ReadAt(hdr[8:16], off+8); err != nil {
				return notSplit(err)
			}
			large := binary.BigEndian.Uint64(hdr[8:16])
			if large > math.MaxInt64 {
				return ErrNotMotionPhoto
			}
			boxSize, headLen = int64(large), 16
		}
		// A broken box: not a file this package can split.
		if boxSize < headLen || boxSize > size-off {
			return ErrNotMotionPhoto
		}
		if n == 0 && typ != "ftyp" {
			return ErrNotMotionPhoto
		}
		stop, err := fn(off, boxSize, headLen, typ)
		if err != nil || stop {
			return err
		}
		off += boxSize
	}
	return nil
}

// analyzeHEIF returns ErrNotMotionPhoto when f is not a HEIF file with an
// embedded video: no mpvd box, or one that does not hold an MP4.
func analyzeHEIF(f *os.File) (*heifLayout, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var l *heifLayout
	err = walkBoxes(f, st.Size(), func(off, boxSize, headLen int64, typ string) (bool, error) {
		if typ != "mpvd" {
			return false, nil
		}
		// Only accept it when an MP4 (or QuickTime) file really starts there.
		if boxSize-headLen < 8 {
			return true, ErrNotMotionPhoto
		}
		box := make([]byte, 8)
		if _, err := f.ReadAt(box, off+headLen); err != nil {
			return true, notSplit(err)
		}
		if string(box[4:8]) != "ftyp" {
			return true, ErrNotMotionPhoto
		}
		l = &heifLayout{size: st.Size(), box: boxSize, videoOff: off + headLen, videoLen: boxSize - headLen, modTime: st.ModTime()}
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, ErrNotMotionPhoto
	}
	return l, nil
}

// maxSamsungIndex bounds the size of a Samsung index box that is read.
const maxSamsungIndex = 1 << 20

// disableSamsungIndex turns the box in which Samsung phones index what a
// file holds ("sefd") into a "free" box, when it lists the embedded video
// (MotionPhoto_Data): ExifTool keeps it, and still reports an embedded video
// from it (EmbeddedVideoFile, a stub), which a reader of those tags may take
// for the video. It keeps its size, so
// nothing else in the file moves; every reader skips a free box. The rest of
// that index (Samsung's own capture time and camera mode) goes with it; the
// EXIF dates are not in it.
func disableSamsungIndex(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	var found []int64
	err = walkBoxes(f, st.Size(), func(off, boxSize, headLen int64, typ string) (bool, error) {
		if typ != "sefd" || boxSize-headLen > maxSamsungIndex {
			return false, nil
		}
		payload := make([]byte, boxSize-headLen)
		if _, err := f.ReadAt(payload, off+headLen); err != nil {
			return true, err
		}
		if bytes.Contains(payload, []byte("MotionPhoto_Data")) {
			found = append(found, off)
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	for _, off := range found {
		if _, err := f.WriteAt([]byte("free"), off+4); err != nil {
			return err
		}
	}
	return f.Close()
}

// ExtractVideoHEIF copies the embedded video of the HEIF motion photo at src
// to dst. It returns ErrNotMotionPhoto (and writes nothing) when src is not
// one.
func ExtractVideoHEIF(src, dst string) (*Info, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l, err := analyzeHEIF(f)
	if err != nil {
		return nil, err
	}
	out, finish, err := create(dst, l.modTime)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(out, io.NewSectionReader(f, l.videoOff, l.videoLen))
	if ferr := finish(err == nil); err == nil {
		err = ferr
	}
	if err != nil {
		return nil, err
	}
	return &Info{VideoBytes: l.videoLen, StillBytes: l.size - l.box}, nil
}

// StripHEIF writes to dst a copy of the HEIF motion photo at src without its
// video and without the metadata that announces one, so Immich does not look
// for a video that is no longer there. It needs ExifTool. It returns
// ErrNotMotionPhoto (and writes nothing) when src is not a HEIF motion photo.
func StripHEIF(ctx context.Context, src, dst string) (*Info, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	l, err := analyzeHEIF(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil, fmt.Errorf("%s already exists", dst)
	}

	tool, err := exec.LookPath(exiftool)
	if err != nil {
		return nil, fmt.Errorf("removing the video of a HEIF motion photo needs ExifTool: %w", err)
	}
	cmd := exec.CommandContext(ctx, tool, "-q", "-m",
		"-MotionPhotoVideo=",
		"-XMP-GCamera:MotionPhoto=", "-XMP-GCamera:MotionPhotoVersion=", "-XMP-GCamera:MotionPhotoPresentationTimestampUs=",
		"-XMP-GContainer:all=",
		"-o", dst, src)
	cmd.Env = utils.EnvWithoutSecrets(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dst)
		return nil, fmt.Errorf("exiftool could not remove the video: %w: %s", err, tail(out.String()))
	}
	if err := disableSamsungIndex(dst); err != nil {
		_ = os.Remove(dst)
		return nil, fmt.Errorf("removing Samsung's index of the video: %w", err)
	}
	size, err := checkStripped(dst, l)
	if err != nil {
		_ = os.Remove(dst)
		return nil, fmt.Errorf("the photo without its video is not usable: %w", err)
	}
	// Keep the original modification time: sources use it as the asset date.
	_ = os.Chtimes(dst, l.modTime, l.modTime)
	return &Info{VideoBytes: l.videoLen, StillBytes: size}, nil
}

// checkStripped returns the size of the photo without its video, or why it
// is not one: still a HEIF file, with no video, and smaller than the original.
func checkStripped(path string, l *heifLayout) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	head := make([]byte, 8)
	if _, err := io.ReadFull(f, head); err != nil || !isHEIF(head) {
		return 0, errors.New("it is not a HEIF file")
	}
	if _, err := analyzeHEIF(f); !errors.Is(err, ErrNotMotionPhoto) {
		if err != nil {
			return 0, err
		}
		return 0, errors.New("the video is still in it")
	}
	// The metadata may be rewritten a little larger; the video is not there.
	if st.Size() >= l.size {
		return 0, fmt.Errorf("it is %d bytes, not smaller than the %d of the original", st.Size(), l.size)
	}
	return st.Size(), nil
}

// notSplit is the answer for a read that stopped early: ErrNotMotionPhoto
// when the file just ended, else the error.
func notSplit(err error) error {
	if err := readErr(err); err != nil {
		return err
	}
	return ErrNotMotionPhoto
}

// tail is the end of a tool's output, for an error message.
func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}
