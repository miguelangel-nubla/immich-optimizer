package mediacheck

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubTools puts stand-ins for vips, vipsheader, ffprobe and ffmpeg first in
// PATH, so the decisions of this package are tested without the real tools
// (which only the Docker image has). A stand-in reads what it should answer
// from the "media" file it is given, which is a line of text:
//
//	image 6000x4000 photo     an image of that size showing a picture
//	image 160x120 flat        an image that is one colour
//	video 1920x1080 10 photo  a video of that size and length in seconds
//	video 0x0 0 photo         a video without picture track and length
//	broken                    a file no tool can read
func stubTools(t *testing.T) (photo, flat string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-ins")
	}
	dir := t.TempDir()
	writePNG := func(name string, gradient bool) string {
		img := image.NewRGBA(image.Rect(0, 0, 32, 32))
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				c := color.RGBA{120, 120, 120, 255}
				if gradient {
					c = color.RGBA{uint8(x * 8), uint8(y * 8), 90, 255}
				}
				img.Set(x, y, c)
			}
		}
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, img); err != nil {
			t.Fatal(err)
		}
		return p
	}
	photo, flat = writePNG("photo.png", true), writePNG("flat.png", false)
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil { // #nosec G306 -- a test's stand-in tool
			t.Fatal(err)
		}
	}
	// vipsheader -f width|height FILE
	script("vipsheader", `read kind size look < "$3" || true
[ "$kind" = image ] || { echo "not an image" >&2; exit 1; }
case "$2" in width) echo "${size%x*}";; height) echo "${size#*x}";; esac
`)
	// vips thumbnail SRC DST ...
	script("vips", `read kind size look < "$2" || true
[ "$kind" = image ] || { echo "cannot decode" >&2; exit 1; }
dst="${3%%\[*}"
cp "`+dir+`/$look.png" "$dst"
`)
	// ffprobe ... FILE (last argument); what is asked is in the arguments.
	script("ffprobe", `for last; do :; done
read kind size secs look < "$last" || true
[ "$kind" = video ] || { echo "not a video" >&2; exit 1; }
case "$*" in
  *stream=width,height*) [ "$size" = 0x0 ] || echo "$size";;
  *stream=codec_name*) [ "$size" = 0x0 ] || echo h264;;
  *format=duration*) echo "$secs";;
esac
`)
	// ffmpeg ... -i SRC ... DST (last argument)
	script("ffmpeg", `for last; do :; done
src=; prev=
for a; do [ "$prev" = -i ] && src="$a"; prev="$a"; done
read kind size secs look < "$src" || true
[ "$kind" = video ] || exit 1
cp "`+dir+`/$look.png" "$last"
`)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return photo, flat
}

func media(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheckSize(t *testing.T) {
	stubTools(t)
	c, ctx := New(), context.Background()
	raw := media(t, "a.dng", "image 6000x4000 photo")
	cases := []struct {
		name, original, output string
		refused                bool
	}{
		{"same size", raw, media(t, "a.jxl", "image 6000x4000 photo"), false},
		{"the preview inside a RAW file", raw, media(t, "a.jxl", "image 160x120 photo"), true},
		{"scaled down to a screen", raw, media(t, "a.jxl", "image 1620x1080 photo"), false},
		{"just above the limit", raw, media(t, "a.jxl", "image 376x251 photo"), false},
		{"just below the limit", raw, media(t, "a.jxl", "image 374x249 photo"), true},
		{"rotated", raw, media(t, "a.jxl", "image 4000x6000 photo"), false},
		{"rotated and tiny", raw, media(t, "a.jxl", "image 120x160 photo"), true},
		{"cropped to a strip", raw, media(t, "a.jxl", "image 6000x200 photo"), false},
		{"a video", media(t, "a.mov", "video 3840x2160 10 photo"), media(t, "a.mp4", "video 1920x1080 10 photo"), false},
		{"a tiny video", media(t, "a.mov", "video 3840x2160 10 photo"), media(t, "a.mp4", "video 160x90 10 photo"), true},
		{"a video named like a picture", media(t, "spooled", "video 3840x2160 10 photo"), media(t, "a.jxl", "image 160x90 photo"), true},
		{"an original nothing can read", media(t, "a.xyz", "broken"), media(t, "a.jxl", "image 16x16 photo"), false},
		{"an output nothing can read", raw, media(t, "a.jxl", "broken"), false},
	}
	for _, tc := range cases {
		err := c.CheckSize(ctx, tc.original, tc.output)
		if (err != nil) != tc.refused {
			t.Errorf("%s: err = %v, want refused = %v", tc.name, err, tc.refused)
		}
	}
	if err := c.CheckSize(ctx, raw, cases[1].output); err == nil || !strings.Contains(err.Error(), "160x120") || !strings.Contains(err.Error(), "6000x4000") {
		t.Errorf("the error does not give both sizes: %v", err)
	}
}

func TestCheckDecidesWhatIsAValidResult(t *testing.T) {
	stubTools(t)
	c, ctx := New(), context.Background()
	photo := media(t, "a.jpg", "image 4000x3000 photo")
	clip := media(t, "a.mov", "video 1920x1080 10 photo")
	cases := []struct {
		name, original, output string
		valid                  bool
	}{
		{"a picture", photo, media(t, "a.jxl", "image 4000x3000 photo"), true},
		{"a picture that does not decode", photo, media(t, "a.jxl", "broken"), false},
		{"a picture that came out flat", photo, media(t, "a.jxl", "image 4000x3000 flat"), false},
		{"a flat picture of a flat original", media(t, "a.jpg", "image 4000x3000 flat"), media(t, "a.jxl", "image 4000x3000 flat"), true},
		{"a flat picture of an original nothing can read", media(t, "a.jpg", "broken"), media(t, "a.jxl", "image 4000x3000 flat"), false},
		{"a video", clip, media(t, "a.mp4", "video 1920x1080 10 photo"), true},
		{"a video without a picture track", clip, media(t, "a.mp4", "video 0x0 10 photo"), false},
		{"a video without length", clip, media(t, "a.mp4", "video 1920x1080 0 photo"), false},
		{"a video that cannot be read", clip, media(t, "a.mp4", "broken"), false},
		{"a video that came out black", clip, media(t, "a.mp4", "video 1920x1080 10 flat"), false},
		{"a black video of a black original", media(t, "a.mov", "video 1920x1080 10 flat"), media(t, "a.mp4", "video 1920x1080 10 flat"), true},
		{"a motion photo turned into its video", photo, media(t, "a.mp4", "video 1920x1080 3 photo"), true},
	}
	for _, tc := range cases {
		err := c.Check(ctx, tc.original, tc.output)
		if (err == nil) != tc.valid {
			t.Errorf("%s: err = %v, want valid = %v", tc.name, err, tc.valid)
		}
	}
}
