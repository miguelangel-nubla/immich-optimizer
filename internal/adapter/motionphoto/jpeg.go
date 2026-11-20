// Package motionphoto implements the "keep the photo, drop the video" policy
// for Android motion photos: a JPEG with an MP4 appended after the image data
// and XMP that announces it (Google "MotionPhoto" with a Container directory,
// or the older "MicroVideo" form).
//
// StripJPEG writes a copy of the still without the trailing video and with
// the motion flags switched off, so Immich does not look for a video that is
// no longer there. The input is never modified.
package motionphoto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
)

// Info describes what StripJPEG removed.
type Info struct {
	// VideoBytes is the size of the removed trailing video.
	VideoBytes int64
	// StillBytes is the size of the written still.
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

// StripJPEG copies the still image of the motion photo at src to dst. It
// returns ErrNotMotionPhoto (and writes nothing) when src is not a JPEG
// motion photo, or when the announced video cannot be confirmed at the end of
// the file.
func StripJPEG(src, dst string) (*Info, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()

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

	videoLen := videoLength(xmp)
	if videoLen <= 0 || videoLen >= size {
		return nil, ErrNotMotionPhoto
	}
	cut := size - videoLen

	// Only cut when an MP4 really starts there ("ftyp" box).
	box := make([]byte, 8)
	if _, err := f.ReadAt(box, cut); err != nil || string(box[4:8]) != "ftyp" {
		return nil, ErrNotMotionPhoto
	}

	// Switch the flags off in place ("1" -> "0"): same length, so no JPEG
	// segment has to be rewritten.
	patched := bytes.Clone(xmp)
	for _, m := range flags {
		for _, g := range []int{2, 4} {
			if m[g] >= 0 {
				patched[m[g]] = '0'
			}
		}
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(dst)
		}
	}()

	w := bufio.NewWriterSize(out, 1<<20)
	if _, err := io.Copy(w, io.NewSectionReader(f, 0, xmpOff)); err != nil {
		return nil, err
	}
	if _, err := w.Write(patched); err != nil {
		return nil, err
	}
	rest := xmpOff + int64(len(xmp))
	if _, err := io.Copy(w, io.NewSectionReader(f, rest, cut-rest)); err != nil {
		return nil, err
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	// Keep the original modification time: sources use it as the asset date.
	_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	ok = true
	return &Info{VideoBytes: videoLen, StillBytes: cut}, nil
}

// videoLength returns the number of trailing bytes that belong to the video,
// or 0 when the XMP does not say.
func videoLength(xmp []byte) int64 {
	for _, item := range itemRe.FindAll(xmp, -1) {
		if !semanticRe.Match(item) {
			continue
		}
		n := firstInt(lengthRe, item)
		if n <= 0 {
			continue
		}
		return n + firstInt(paddingRe, item)
	}
	return firstInt(microOffsetRe, xmp)
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
// returns nil when the file is not a JPEG or has no XMP.
func findXMP(f *os.File) (int64, []byte, error) {
	r := io.NewSectionReader(f, 0, maxHeaderScan)
	br := bufio.NewReader(r)
	var pos int64

	soi := make([]byte, 2)
	if _, err := io.ReadFull(br, soi); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 0, nil, nil
	}
	pos = 2

	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return 0, nil, nil
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
		if segLen < 2 {
			return 0, nil, fmt.Errorf("invalid JPEG segment length %d", segLen)
		}
		payloadLen := segLen - 2
		payloadOff := pos + 4

		if marker == 0xE1 && payloadLen > int64(len(xmpHeader)) {
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(br, payload); err != nil {
				return 0, nil, nil
			}
			if bytes.HasPrefix(payload, []byte(xmpHeader)) {
				return payloadOff + int64(len(xmpHeader)), payload[len(xmpHeader):], nil
			}
		} else if _, err := br.Discard(int(payloadLen)); err != nil {
			return 0, nil, nil
		}
		pos = payloadOff + payloadLen
	}
}
