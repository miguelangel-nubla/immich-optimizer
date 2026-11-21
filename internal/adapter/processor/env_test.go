package processor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// A command can be written with environment variables alone, and that is the
// only way it learns the file's own name.
func TestCommandEnvironment(t *testing.T) {
	src := filepath.Join(t.TempDir(), "upload.jpg")
	if err := os.WriteFile(src, []byte("a photo, long enough to be bigger than the answer............................................................"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No template value at all.
	task := entity.Task{Name: "env-only", Extensions: []string{"jpg"}, MotionPhotos: entity.MotionPhotosKeep,
		Command: `test -f "$IO_SRC_FOLDER/$IO_NAME.$IO_EXTENSION" && printf '%s|%s|%s|%s' "$IO_ORIGINAL_NAME" "$IO_SOURCE" "$IO_TASK" "$IO_EXTENSION" > "$IO_DST_FOLDER/$IO_NAME.txt"`}

	// A name that would run something if it ever reached the command line.
	const nasty = `it's "$(touch /tmp/io-pwned)" ` + "`id`" + `; rm -rf x.jpg`
	ctx := entity.WithTaskInfo(context.Background(), entity.TaskInfo{OriginalName: nasty, Source: "proxy"})
	res, err := NewLocalProcessor(nil, "").Process(ctx, src, []entity.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	got, err := os.ReadFile(res.ProcessedFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := nasty + "|proxy|env-only|jpg"; string(got) != want {
		t.Errorf("the command saw\n%s\nwant\n%s", got, want)
	}

	// Without a caller that knows better, the name is the file's.
	res2, err := NewLocalProcessor(nil, "").Process(context.Background(), src, []entity.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Cleanup()
	if got, _ := os.ReadFile(res2.ProcessedFilePath); !strings.HasPrefix(string(got), "upload.jpg||env-only|") {
		t.Errorf("the command saw %s", got)
	}
}
