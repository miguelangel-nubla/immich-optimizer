// Package mediacheck decides whether a converted file looks like a valid
// photo or video. It catches the common ways a conversion goes wrong while
// still exiting cleanly: an empty or truncated file, an image that does not
// decode, a picture that came out as a single flat colour, a video with no
// picture or no length.
//
// It does not compare the output with the original picture: a task may crop,
// resize, watermark or re-encode lossily, and all of that is fine. The
// original is only looked at to allow an output that is flat because the
// original already was (a black frame stays a black frame).
//
// Images are decoded with vips and videos with ffprobe/ffmpeg, the tools the
// optimizer images ship.
package mediacheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/png" // thumbnails are written as PNG
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

// flatStdDev is the brightness spread (0-255 scale) below which a picture
// counts as one flat colour.
const flatStdDev = 1.0

type Checker struct{}

func New() *Checker { return &Checker{} }

var videoExt = map[string]bool{}

func init() {
	for _, e := range strings.Fields("3gp 3gpp avi flv insv m2t m2ts m4v mkv mov mp4 mpe mpeg mpg mts mxf ts vob webm wmv") {
		videoExt["."+e] = true
	}
}

func isVideo(path string) bool { return videoExt[strings.ToLower(filepath.Ext(path))] }

// Check returns nil when output looks like a valid photo or video.
func (c *Checker) Check(ctx context.Context, original, output string) error {
	st, err := os.Stat(output)
	if err != nil {
		return fmt.Errorf("converted file: %w", err)
	}
	if st.Size() == 0 {
		return errors.New("converted file is empty")
	}
	tmp, err := os.MkdirTemp("", "io-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	render := renderImage
	what := "image"
	if isVideo(output) {
		render, what = renderVideoFrame, "video"
		if err := checkVideoStream(ctx, output); err != nil {
			return err
		}
	}
	img, err := render(ctx, output, filepath.Join(tmp, "output.png"))
	if err != nil {
		return fmt.Errorf("converted %s cannot be decoded: %w", what, err)
	}
	if !Flat(img) {
		return nil
	}
	// A flat output is only acceptable when the original is flat too.
	origRender := renderImage
	if isVideo(original) {
		origRender = renderVideoFrame
	}
	if orig, err := origRender(ctx, original, filepath.Join(tmp, "original.png")); err == nil && Flat(orig) {
		return nil
	}
	return fmt.Errorf("converted %s is a single flat colour", what)
}

// minScale is how much smaller than the original, in width and in height, a
// converted picture may be: one sixteenth. A task may resize, and scaling a
// large photo down to a screen's size stays far above this; a result below it
// is not a smaller version of the picture but something else, such as the
// preview stored inside a camera RAW file, which is what a tool that cannot
// develop RAW reads from one.
const minScale = 16

// CheckSize returns an error when the converted picture or video is tiny next
// to its original. It says nothing (nil) when either size cannot be read:
// whether the output is valid at all is Check's question.
func (c *Checker) CheckSize(ctx context.Context, original, output string) error {
	ow, oh, ok := pixelSize(ctx, original)
	if !ok {
		return nil
	}
	w, h, ok := pixelSize(ctx, output)
	if !ok {
		return nil
	}
	// Longer side with longer side: a task may rotate.
	if max(w, h)*minScale < max(ow, oh) && min(w, h)*minScale < min(ow, oh) {
		return fmt.Errorf("the converted file is %dx%d, the original %dx%d: less than 1/%d of its width and height is not taken for the same picture", w, h, ow, oh, minScale)
	}
	return nil
}

// pixelSize returns the width and height of a photo or a video, whatever its
// file name says it is.
func pixelSize(ctx context.Context, path string) (w, h int, ok bool) {
	if out, err := run(ctx, "vipsheader", "-f", "width", path); err == nil {
		if w, err = strconv.Atoi(out); err == nil {
			if out, err = run(ctx, "vipsheader", "-f", "height", path); err == nil {
				if h, err = strconv.Atoi(out); err == nil && w > 0 && h > 0 {
					return w, h, true
				}
			}
		}
	}
	out, err := run(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", path)
	if err != nil {
		return 0, 0, false
	}
	ws, hs, found := strings.Cut(strings.TrimSpace(strings.Split(out, "\n")[0]), "x")
	if !found {
		return 0, 0, false
	}
	w, err = strconv.Atoi(ws)
	if err != nil {
		return 0, 0, false
	}
	h, err = strconv.Atoi(strings.TrimRight(hs, "x"))
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// Flat reports whether the picture is (nearly) one single colour, or empty.
func Flat(img image.Image) bool {
	b := img.Bounds()
	n := float64(b.Dx() * b.Dy())
	if n == 0 {
		return true
	}
	var sum, sumSq float64
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			l := (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257
			sum += l
			sumSq += l * l
		}
	}
	mean := sum / n
	return math.Sqrt(math.Max(0, sumSq/n-mean*mean)) < flatStdDev
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	// The decoders read untrusted files: they get no secrets.
	cmd.Env = utils.EnvWithoutSecrets(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return "", fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func decodePNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	if b := img.Bounds(); b.Dx() == 0 || b.Dy() == 0 {
		return nil, errors.New("picture has no size")
	}
	return img, nil
}

// renderImage fully decodes an image into a small thumbnail.
func renderImage(ctx context.Context, src, dst string) (image.Image, error) {
	// --fail-on truncated: a file that stops halfway is an error, not a
	// picture with a grey lower half.
	if _, err := run(ctx, "vips", "thumbnail", src, dst, "128", "--fail-on", "truncated"); err != nil {
		return nil, err
	}
	return decodePNG(dst)
}

func videoDuration(ctx context.Context, path string) (float64, error) {
	out, err := run(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path)
	if err != nil {
		return 0, err
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		return 0, fmt.Errorf("unreadable duration %q", out)
	}
	return d, nil
}

func checkVideoStream(ctx context.Context, path string) error {
	out, err := run(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path)
	if err != nil {
		return fmt.Errorf("converted video cannot be read: %w", err)
	}
	if out == "" {
		return errors.New("converted video has no picture track")
	}
	d, err := videoDuration(ctx, path)
	if err != nil {
		return fmt.Errorf("converted video cannot be read: %w", err)
	}
	if d <= 0 {
		return errors.New("converted video has no length")
	}
	return nil
}

// renderVideoFrame decodes the frame in the middle of a video.
func renderVideoFrame(ctx context.Context, src, dst string) (image.Image, error) {
	d, err := videoDuration(ctx, src)
	if err != nil {
		return nil, err
	}
	at := strconv.FormatFloat(d/2, 'f', 3, 64)
	if _, err := run(ctx, "ffmpeg", "-v", "error", "-y", "-ss", at, "-i", src, "-frames:v", "1", "-vf", "scale=128:-2", dst); err != nil {
		return nil, err
	}
	return decodePNG(dst)
}
