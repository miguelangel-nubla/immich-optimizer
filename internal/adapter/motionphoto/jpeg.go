// Package motionphoto splits motion photos, a photo with a short video in the
// same file, into the photo and the video:
//
//   - JPEG: an MP4 appended after the image data, announced by XMP (Google
//     "MotionPhoto" with a Container directory, or the older "MicroVideo"
//     form). Done here entirely.
//   - HEIF (HEIC, AVIF): the video in a top-level "mpvd" box. Found and
//     copied out here; removed with ExifTool (see heif.go).
//
// Strip writes a copy of the photo without the video and with the motion
// flags switched off, so Immich does not look for a video that is no longer
// there. ExtractVideo writes only the video. The input is never modified.
package motionphoto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"
)

// Info describes the two parts of a motion photo.
type Info struct {
	// VideoBytes is the size of the video. What may follow it in the file
	// (a Samsung trailer) is neither photo nor video.
	VideoBytes int64
	// StillBytes is the size of the still.
	StillBytes int64
}

// ErrNotMotionPhoto is returned when the file is not a JPEG motion photo this
// package understands. It is the normal answer for ordinary files.
var ErrNotMotionPhoto = errors.New("not a motion photo")

const (
	xmpHeader = "http://ns.adobe.com/xap/1.0/\x00"
	// maxHeaderScan bounds how far into the file the XMP segment is searched.
	maxHeaderScan = 8 << 20
)

var (
	// GCamera:MotionPhoto="1" / <GCamera:MotionPhoto>1</GCamera:MotionPhoto>,
	// and the same for MicroVideo. Group 1 or 2 is the "1" to switch off.
	flagRe = regexp.MustCompile(`:(?:MotionPhoto|MicroVideo)(?:\s*=\s*["'](1)["']|>\s*(1)\s*<)`)

	microOffsetRe = regexp.MustCompile(`:MicroVideoOffset(?:\s*=\s*["'](\d+)["']|>\s*(\d+)\s*<)`)

	// One <Container:Item .../> element of the Container directory.
	itemRe     = regexp.MustCompile(`(?s)<[\w-]+:Item\b[^>]*>`)
	semanticRe = regexp.MustCompile(`:Semantic\s*=\s*["']MotionPhoto["']`)
	lengthRe   = regexp.MustCompile(`:Length\s*=\s*["'](\d+)["']`)
	paddingRe  = regexp.MustCompile(`:Padding\s*=\s*["'](\d+)["']`)
)

// layout is what analyze learns about a JPEG motion photo.
type layout struct {
	size    int64
	cut     int64 // where the video starts
	end     int64 // where it ends: a trailer may follow (Samsung's)
	xmpOff  int64
	xmp     []byte
	flags   [][]int
	modTime time.Time
}

// analyze returns ErrNotMotionPhoto when f is not a JPEG motion photo, or
// when the announced video cannot be confirmed at the end of the file.
func analyze(f *os.File) (*layout, error) {
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()

	xmpOff, xmp, err := findXMP(f)
	if err != nil {
		return nil, err
	}
	if xmp == nil {
		return nil, ErrNotMotionPhoto
	}
	flags := flagRe.FindAllSubmatchIndex(xmp, -1)
	if len(flags) == 0 {
		return nil, ErrNotMotionPhoto
	}
	// The video starts Length bytes before the end of the file. What it
	// declares as padding after it, or a Samsung trailer, is not video.
	videoLen, padding := videoLength(xmp)
	if videoLen <= 0 || videoLen >= size {
		return nil, ErrNotMotionPhoto
	}
	cut := size - videoLen
	trailer := max(padding, samsungTrailer(f, size))
	if trailer < 0 || trailer > videoLen-8 {
		trailer = 0
	}
	// The video comes after the photo, never inside its metadata.
	if cut < xmpOff+int64(len(xmp)) {
		return nil, ErrNotMotionPhoto
	}

	// Only accept it when an MP4 really starts there ("ftyp" box).
	box := make([]byte, 8)
	if _, err := f.ReadAt(box, cut); err != nil || string(box[4:8]) != "ftyp" {
		return nil, ErrNotMotionPhoto
	}
	return &layout{size: size, cut: cut, end: size - trailer, xmpOff: xmpOff, xmp: xmp, flags: flags, modTime: st.ModTime()}, nil
}

// samsungTrailer returns the length of the trailer Samsung phones append to
// a motion photo (an index of what the file holds, "SEFH" ... "SEFT"), or 0.
// It ends with its own length (little-endian) and "SEFT".
func samsungTrailer(f *os.File, size int64) int64 {
	if size < 16 {
		return 0
	}
	tail := make([]byte, 8)
	if _, err := f.ReadAt(tail, size-8); err != nil || string(tail[4:]) != "SEFT" {
		return 0
	}
	n := int64(binary.LittleEndian.Uint32(tail[:4])) + 8
	if n < 16 || n > size {
		return 0
	}
	head := make([]byte, 4)
	if _, err := f.ReadAt(head, size-n); err != nil || string(head) != "SEFH" {
		return 0
	}
	return n
}

// create opens dst for writing and returns a finish function that closes it,
// applies modTime, and removes the file unless called with ok=true.
func create(dst string, modTime time.Time) (*os.File, func(ok bool) error, error) {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return out, func(ok bool) error {
		err := out.Close()
		if !ok || err != nil {
			os.Remove(dst)
			return err
		}
		// Keep the original modification time: sources use it as the asset date.
		_ = os.Chtimes(dst, modTime, modTime)
		return nil
	}, nil
}

// IsMotionJPEG reports whether the file is a JPEG motion photo this package
// can split.
//
// A file that cannot be read counts as one: whoever asks then leaves it as
// it is rather than risk hiding a video.
func IsMotionJPEG(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	_, err = analyze(f)
	return !errors.Is(err, ErrNotMotionPhoto)
}

// StripJPEG copies the still image of the motion photo at src to dst, without
// the video and with the motion flags switched off. It returns
// ErrNotMotionPhoto (and writes nothing) when src is not a JPEG motion photo.
func StripJPEG(src, dst string) (*Info, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l, err := analyze(f)
	if err != nil {
		return nil, err
	}

	// Switch the flags off in place ("1" -> "0"): same length, so no JPEG
	// segment has to be rewritten.
	patched := bytes.Clone(l.xmp)
	for _, m := range l.flags {
		for _, g := range []int{2, 4} {
			if m[g] >= 0 {
				patched[m[g]] = '0'
			}
		}
	}

	out, finish, err := create(dst, l.modTime)
	if err != nil {
		return nil, err
	}
	w := bufio.NewWriterSize(out, 1<<20)
	rest := l.xmpOff + int64(len(l.xmp))
	_, err = io.Copy(w, io.NewSectionReader(f, 0, l.xmpOff))
	if err == nil {
		_, err = w.Write(patched)
	}
	if err == nil {
		_, err = io.Copy(w, io.NewSectionReader(f, rest, l.cut-rest))
	}
	if err == nil {
		err = w.Flush()
	}
	if ferr := finish(err == nil); err == nil {
		err = ferr
	}
	if err != nil {
		return nil, err
	}
	return &Info{VideoBytes: l.end - l.cut, StillBytes: l.cut}, nil
}

// ExtractVideoJPEG copies the embedded MP4 of the motion photo at src to dst.
// It returns ErrNotMotionPhoto (and writes nothing) when src is not a JPEG
// motion photo.
func ExtractVideoJPEG(src, dst string) (*Info, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	l, err := analyze(f)
	if err != nil {
		return nil, err
	}
	out, finish, err := create(dst, l.modTime)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(out, io.NewSectionReader(f, l.cut, l.end-l.cut))
	if ferr := finish(err == nil); err == nil {
		err = ferr
	}
	if err != nil {
		return nil, err
	}
	return &Info{VideoBytes: l.end - l.cut, StillBytes: l.cut}, nil
}

// videoLength returns how many bytes before the end of the file the video
// starts, and the padding declared after it; 0 when the XMP does not say.
//
// The video item's Length counts from its start to the end of the file.
// Samsung phones count their trailer in it too, and give its size as the
// item's Padding (One UI 6), or not at all (One UI 5).
func videoLength(xmp []byte) (length, padding int64) {
	for _, item := range itemRe.FindAll(xmp, -1) {
		if !semanticRe.Match(item) {
			continue
		}
		n := firstInt(lengthRe, item)
		if n <= 0 {
			continue
		}
		return n, firstInt(paddingRe, item)
	}
	return firstInt(microOffsetRe, xmp), 0
}

func firstInt(re *regexp.Regexp, b []byte) int64 {
	m := re.FindSubmatch(b)
	if m == nil {
		return 0
	}
	for _, g := range m[1:] {
		if len(g) > 0 {
			n, err := strconv.ParseInt(string(g), 10, 64)
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// findXMP walks the JPEG segments before the image data and returns the file
// offset and payload of the XMP packet (without the namespace header). It
// returns nil when the file is not a JPEG or has no XMP, and an error only
// when the file could not be read.
func findXMP(f *os.File) (int64, []byte, error) {
	r := io.NewSectionReader(f, 0, maxHeaderScan)
	br := bufio.NewReader(r)
	var pos int64

	soi := make([]byte, 2)
	if _, err := io.ReadFull(br, soi); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 0, nil, readErr(err)
	}
	pos = 2

	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return 0, nil, readErr(err)
		}
		if hdr[0] != 0xFF {
			return 0, nil, nil
		}
		marker := hdr[1]
		// Start of scan or end of image: the metadata segments are over.
		if marker == 0xDA || marker == 0xD9 {
			return 0, nil, nil
		}
		segLen := int64(binary.BigEndian.Uint16(hdr[2:4]))
		// A broken segment: not a file this package can split.
		if segLen < 2 {
			return 0, nil, nil
		}
		payloadLen := segLen - 2
		payloadOff := pos + 4

		if marker == 0xE1 && payloadLen > int64(len(xmpHeader)) {
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(br, payload); err != nil {
				return 0, nil, readErr(err)
			}
			if bytes.HasPrefix(payload, []byte(xmpHeader)) {
				return payloadOff + int64(len(xmpHeader)), payload[len(xmpHeader):], nil
			}
		} else if _, err := br.Discard(int(payloadLen)); err != nil {
			return 0, nil, readErr(err)
		}
		pos = payloadOff + payloadLen
	}
}

// readErr is the error of a read that stopped early: none when the file
// just ended (it is then not a file this package splits), else the error.
func readErr(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}
