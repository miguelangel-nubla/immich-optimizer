package motionphoto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"

	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

// HDR photos (Ultra HDR, ISO 21496-1): a JPEG followed by a second, small
// JPEG, the gain map, that HDR screens use to show the bright parts brighter.
// The photo announces it in its XMP (hdrgm:Version, a GainMap item in the
// Container directory) or with an ISO 21496-1 segment; an MPF segment says
// where the gain map is.

var (
	hdrgmRe        = regexp.MustCompile(`hdrgm:Version`)
	gainSemanticRe = regexp.MustCompile(`:Semantic\s*=\s*["']GainMap["']`)
)

// isoGainMapID starts the APP2 segment of an ISO 21496-1 gain map photo.
const isoGainMapID = "urn:iso:std:iso:ts:21496:-1\x00"

// ErrNoGainMap is returned when the file is not a photo with a gain map.
var ErrNoGainMap = errors.New("not a photo with a gain map")

// HasGainMap reports whether the file is a JPEG that announces an HDR gain
// map. A file that cannot be read counts as one: whoever asks then handles it
// as the more delicate kind.
func HasGainMap(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	has, err := jpegHasGainMap(f)
	return has || err != nil
}

// jpegHasGainMap walks the segments of the photo before its image data.
func jpegHasGainMap(f *os.File) (bool, error) {
	br := bufio.NewReader(io.NewSectionReader(f, 0, maxHeaderScan))
	soi := make([]byte, 2)
	if _, err := io.ReadFull(br, soi); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return false, readErr(err)
	}
	hdr := make([]byte, 4)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return false, readErr(err)
		}
		if hdr[0] != 0xFF || hdr[1] == 0xDA || hdr[1] == 0xD9 {
			return false, nil
		}
		segLen := int(binary.BigEndian.Uint16(hdr[2:4]))
		if segLen < 2 {
			return false, nil
		}
		n := segLen - 2
		switch hdr[1] {
		case 0xE1, 0xE2: // XMP, ISO 21496-1
			payload := make([]byte, n)
			if _, err := io.ReadFull(br, payload); err != nil {
				return false, readErr(err)
			}
			if hdr[1] == 0xE2 && bytes.HasPrefix(payload, []byte(isoGainMapID)) {
				return true, nil
			}
			if hdr[1] == 0xE1 && bytes.HasPrefix(payload, []byte(xmpHeader)) {
				xmp := payload[len(xmpHeader):]
				if hdrgmRe.Match(xmp) || gainSemanticRe.Match(xmp) {
					return true, nil
				}
			}
		default:
			if _, err := br.Discard(n); err != nil {
				return false, readErr(err)
			}
		}
	}
}

// StripGainMap writes to dst the photo at src without its gain map and
// without the metadata that announces one: an ordinary (SDR) photo. It needs
// ExifTool. It returns ErrNoGainMap (and writes nothing) when src has no gain
// map, and an error when src is a motion photo that still has its video:
// what follows the photo would go with the gain map.
func StripGainMap(ctx context.Context, src, dst string) (*Info, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	has, err := jpegHasGainMap(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	if !has {
		return nil, ErrNoGainMap
	}
	if IsMotionPhoto(src) {
		return nil, errors.New("the photo still has its motion video, which would be removed with the gain map")
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil, fmt.Errorf("%s already exists", dst)
	}
	tool, err := exec.LookPath(exiftool)
	if err != nil {
		return nil, fmt.Errorf("removing the gain map of an HDR photo needs ExifTool: %w", err)
	}
	// MPF says where the gain map is, the XMP and the trailer are the rest
	// of it; the ordinary photo is left as it is.
	cmd := exec.CommandContext(ctx, tool, "-q", "-m",
		"-MPF:all=", "-XMP-hdrgm:all=", "-XMP-GContainer:all=", "-trailer:all=",
		"-o", dst, src)
	cmd.Env = utils.EnvWithoutSecrets(os.Environ())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dst)
		return nil, fmt.Errorf("exiftool could not remove the gain map: %w: %s", err, tail(out.String()))
	}
	res, err := os.Stat(dst)
	if err == nil && res.Size() >= st.Size() {
		err = fmt.Errorf("it is %d bytes, not smaller than the %d of the original", res.Size(), st.Size())
	}
	if err != nil {
		_ = os.Remove(dst)
		return nil, fmt.Errorf("the photo without its gain map is not usable: %w", err)
	}
	_ = os.Chtimes(dst, st.ModTime(), st.ModTime())
	return &Info{StillBytes: res.Size()}, nil
}
