package processor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	customlogger "github.com/miguelangel-nubla/immich-optimizer/internal/logger"
)

func TestLocalProcessor(t *testing.T) {
	tmpDir := t.TempDir()
	watchFile := filepath.Join(tmpDir, "sample.txt")
	if err := os.WriteFile(watchFile, []byte("hello world"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	logger := customlogger.New(log.New(os.Stdout, "", 0), "")
	p := NewLocalProcessor(logger, tmpDir)

	tasks := []entity.Task{
		{
			Name:       "copy-task",
			Extensions: []string{"txt"},
			Command:    "cp {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.txt",
		},
	}

	result, err := p.Process(context.Background(), watchFile, tasks)
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	defer result.Cleanup()

	if result.ProcessedFilePath == "" {
		t.Errorf("Expected ProcessedFilePath to be set")
	}

	if _, err := os.Stat(result.ProcessedFilePath); err != nil {
		t.Errorf("Expected processed file to exist on disk before Cleanup(), got error: %v", err)
	}
}
