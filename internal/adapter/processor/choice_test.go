package processor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// Commands for the tests: one that makes the file bigger, one smaller.
const (
	growCommand   = `cat "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.src_folder}}/{{.name}}.{{.extension}}" > "{{.dst_folder}}/{{.name}}.{{.extension}}"`
	shrinkCommand = `head -c 10 "{{.src_folder}}/{{.name}}.{{.extension}}" > "{{.dst_folder}}/{{.name}}.out"`
)

func inputFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func jpgTask(name, command string) entity.Task {
	return entity.Task{Name: name, Extensions: []string{"jpg"}, Command: command, MotionPhotos: entity.MotionPhotosKeep, GainMaps: entity.GainMapsKeep}
}

// A task whose result is not smaller hands the file to the next task.
func TestNextTaskWhenNotSmaller(t *testing.T) {
	in := inputFile(t, "a.jpg", []byte("a photo of a few dozen bytes, for the tasks to work on"))
	res, err := NewLocalProcessor(nil, "").Process(context.Background(), in, []entity.Task{
		jpgTask("grows", growCommand), jpgTask("shrinks", shrinkCommand),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	if res.Task != "shrinks" || res.ProcessedSize >= res.OriginalSize {
		t.Errorf("result of %s, %d -> %d bytes", res.Task, res.OriginalSize, res.ProcessedSize)
	}
}

// When no later task does better, the first result is the answer (the engine
// then delivers the original), not a failure; its file is removed with it.
func TestNotSmallerResultKeptWhenLaterTasksFail(t *testing.T) {
	in := inputFile(t, "a.jpg", []byte("a photo of a few dozen bytes, for the tasks to work on"))
	res, err := NewLocalProcessor(nil, "").Process(context.Background(), in, []entity.Task{
		jpgTask("grows", growCommand), jpgTask("fails", "exit 3"), jpgTask("grows-too", growCommand),
	})
	if err != nil {
		t.Fatalf("a task that worked became a failure: %v", err)
	}
	if res.Task != "grows" || res.ProcessedSize <= res.OriginalSize {
		t.Errorf("result of %s, %d -> %d bytes", res.Task, res.OriginalSize, res.ProcessedSize)
	}
	if _, err := os.Stat(res.ProcessedFilePath); err != nil {
		t.Fatalf("the kept result is gone: %v", err)
	}
	res.Cleanup()
	if _, err := os.Stat(filepath.Dir(res.ProcessedFilePath)); !os.IsNotExist(err) {
		t.Error("the kept result's folder was left behind")
	}
}

// A task without a command uploads the file as it is: that is a choice, and
// no later task is tried.
func TestPassthroughIsFinal(t *testing.T) {
	in := inputFile(t, "a.jpg", []byte("a photo of a few dozen bytes, for the tasks to work on"))
	res, err := NewLocalProcessor(nil, "").Process(context.Background(), in, []entity.Task{
		jpgTask("as-is", ""), jpgTask("shrinks", shrinkCommand),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	if res.Task != "as-is" {
		t.Errorf("result of %s", res.Task)
	}
}

const gainMapFixture = "../motionphoto/testdata/gainmap.jpg"

// gain_maps: skip leaves HDR photos to the next task; other photos are not
// affected.
func TestGainMapsSkip(t *testing.T) {
	hdr, _ := os.ReadFile(gainMapFixture)
	still, _ := os.ReadFile(filepath.Join(motionFixtures, "still.jpg"))
	lossy := jpgTask("lossy", shrinkCommand)
	lossy.GainMaps = entity.GainMapsSkip
	tasks := []entity.Task{lossy, jpgTask("keeps-hdr", "")}
	for name, c := range map[string]struct {
		data []byte
		want string
	}{"HDR": {hdr, "keeps-hdr"}, "ordinary": {still, "lossy"}} {
		res, err := NewLocalProcessor(nil, "").Process(context.Background(), inputFile(t, "a.jpg", c.data), tasks)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Task != c.want {
			t.Errorf("%s photo went to %s, want %s", name, res.Task, c.want)
		}
		res.Cleanup()
	}
	// Every task skipping: an error that says so.
	_, err := NewLocalProcessor(nil, "").Process(context.Background(), inputFile(t, "a.jpg", hdr), []entity.Task{lossy})
	if err == nil || !strings.Contains(err.Error(), "gain_maps: skip") {
		t.Errorf("err = %v", err)
	}
}

// gain_maps: drop hands the task the ordinary photo.
func TestGainMapsDrop(t *testing.T) {
	if _, err := exec.LookPath("exiftool"); err != nil {
		t.Skip("exiftool is not installed")
	}
	hdr, _ := os.ReadFile(gainMapFixture)
	task := jpgTask("t", "")
	task.GainMaps = entity.GainMapsDrop
	res, err := NewLocalProcessor(nil, "").Process(context.Background(), inputFile(t, "a.jpg", hdr), []entity.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	if !res.GainMapDropped || res.ProcessedSize >= int64(len(hdr)) {
		t.Errorf("dropped %v, %d of %d bytes", res.GainMapDropped, res.ProcessedSize, len(hdr))
	}
	// An ordinary photo is not touched.
	still, _ := os.ReadFile(filepath.Join(motionFixtures, "still.jpg"))
	res2, err := NewLocalProcessor(nil, "").Process(context.Background(), inputFile(t, "b.jpg", still), []entity.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Cleanup()
	if res2.GainMapDropped || res2.ProcessedSize != int64(len(still)) {
		t.Errorf("an ordinary photo was changed: dropped %v, %d bytes", res2.GainMapDropped, res2.ProcessedSize)
	}
}

// Download tasks are for compatibility: the first that works is used, also
// when its result is bigger.
func TestDownloadUsesFirstWorkingTask(t *testing.T) {
	in := inputFile(t, "a.jpg", []byte("a photo of a few dozen bytes, for the tasks to work on"))
	ctx := entity.WithTaskInfo(context.Background(), entity.TaskInfo{Source: entity.SourceDownload})
	res, err := NewLocalProcessor(nil, "").Process(ctx, in, []entity.Task{
		jpgTask("grows", growCommand), jpgTask("shrinks", shrinkCommand),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Cleanup()
	if res.Task != "grows" {
		t.Errorf("download used %s, want the first task", res.Task)
	}
}
