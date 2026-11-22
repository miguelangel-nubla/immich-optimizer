package mediacheck

import (
	"context"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlat(t *testing.T) {
	flat := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for i := range flat.Pix {
		flat.Pix[i] = 128
	}
	if !Flat(flat) {
		t.Error("a single-colour picture must be flat")
	}
	if !Flat(image.NewRGBA(image.Rect(0, 0, 0, 0))) {
		t.Error("an empty picture must be flat")
	}
	photo := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			photo.Set(x, y, color.RGBA{uint8(x * 8), uint8(y * 8), 90, 255})
		}
	}
	if Flat(photo) {
		t.Error("a gradient is not flat")
	}
	// One stray pixel does not make a flat picture a photo.
	flat.Set(3, 3, color.RGBA{140, 140, 140, 255})
	if !Flat(flat) {
		t.Error("nearly flat must still count as flat")
	}
}

func TestCheckRejectsMissingAndEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	c := New()
	if err := c.Check(context.Background(), "x.jpg", filepath.Join(dir, "missing.jxl")); err == nil {
		t.Error("missing output accepted")
	}
	empty := filepath.Join(dir, "empty.jxl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Check(context.Background(), "x.jpg", empty); err == nil {
		t.Error("empty output accepted")
	}
}

// The decoders read untrusted files: the API key is not in their
// environment, the rest is.
func TestRunKeepsSecretsFromTheDecoders(t *testing.T) {
	t.Setenv("IO_IMMICH_API_KEY", "secret-key")
	t.Setenv("IO_IMMICH_API_KEY_FILE", "/run/secret-file")
	t.Setenv("IO_SOMETHING_ELSE", "kept")
	out, err := run(context.Background(), "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "secret-key") || strings.Contains(out, "/run/secret-file") {
		t.Errorf("a decoder got the API key:\n%s", out)
	}
	if !strings.Contains(out, "IO_SOMETHING_ELSE=kept") {
		t.Error("the rest of the environment was lost")
	}
}
