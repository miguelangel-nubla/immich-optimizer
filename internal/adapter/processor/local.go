package processor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/port"
	"github.com/miguelangel-nubla/immich-optimizer/internal/logger"
	"github.com/miguelangel-nubla/immich-optimizer/internal/utils"
)

type localProcessor struct {
	logger    *logger.Logger
	configDir string
}

func NewLocalProcessor(log *logger.Logger, configDir string) port.MediaProcessor {
	return &localProcessor{
		logger:    log,
		configDir: configDir,
	}
}

func (tp *localProcessor) Process(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error) {
	fp := &fileProcessor{
		logger:    tp.logger,
		configDir: tp.configDir,
	}
	res, err := fp.process(ctx, filePath, tasks)
	if err != nil {
		fp.close()
		return nil, err
	}
	if fp.originalFile != nil {
		fp.originalFile.Close()
		fp.originalFile = nil
	}
	if fp.processedFile != nil {
		fp.processedFile.Close()
		fp.processedFile = nil
	}
	res.Cleanup = fp.cleanWorkDir
	return res, nil
}

type fileProcessor struct {
	logger    *logger.Logger
	configDir string

	originalFile      *os.File
	originalFilename  string
	originalExtension string
	originalSize      int64

	tempWorkDir    string
	tempWorkDirSrc string
	tempWorkDirDst string

	processedFile *os.File
}

func (fp *fileProcessor) logf(str string, args ...any) {
	if fp.logger != nil {
		fp.logger.Printf(str, args...)
	}
}

func (fp *fileProcessor) process(ctx context.Context, filePath string, tasks []entity.Task) (*entity.ProcessResult, error) {
	originalFile, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("unable to open file: %w", err)
	}
	fp.originalFile = originalFile

	stat, err := originalFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("unable to get file info: %w", err)
	}

	fp.originalSize = stat.Size()
	fp.originalExtension = strings.ToLower(filepath.Ext(filePath))
	fp.originalFilename = filepath.Base(filePath)

	err = fmt.Errorf("no task found for file extension %s", fp.originalExtension)
	var errors []error

	for _, task := range tasks {
		if !slices.Contains(task.Extensions, utils.NormalizeExtension(fp.originalExtension)) {
			continue
		}

		cmdTmpl, tmplErr := template.New("command").Parse(task.Command)
		if tmplErr != nil {
			errors = append(errors, fmt.Errorf("\ntask %s unable to parse command template: %w", task.Name, tmplErr))
			continue
		}

		convErr := fp.run(ctx, cmdTmpl)
		if convErr != nil {
			errors = append(errors, fmt.Errorf("\ntask %s failed: %w", task.Name, convErr))
			fp.cleanWorkDir()
			continue
		}
		err = nil
		break
	}

	if err != nil {
		if len(errors) > 1 {
			return nil, fmt.Errorf("errors: %v", errors)
		} else if len(errors) == 1 {
			return nil, errors[0]
		}
		return nil, err
	}

	return fp.processResults()
}

func (fp *fileProcessor) close() {
	if fp.originalFile != nil {
		if err := fp.originalFile.Close(); err != nil {
			fp.logf("unable to close original file: %v", err)
		}
	}

	if fp.processedFile != nil {
		if err := fp.processedFile.Close(); err != nil {
			fp.logf("unable to close processed file: %v", err)
		}
	}

	fp.cleanWorkDir()
}

func (fp *fileProcessor) cleanWorkDir() {
	if fp.tempWorkDir != "" {
		if err := os.RemoveAll(fp.tempWorkDir); err != nil {
			fp.logf("unable to clean temp folder: %v", err)
		}
	}

	fp.tempWorkDir = ""
	fp.tempWorkDirSrc = ""
	fp.tempWorkDirDst = ""
}

func (fp *fileProcessor) run(ctx context.Context, commandTemplate *template.Template) error {
	if err := fp.setupWorkDirectories(); err != nil {
		return err
	}

	tempFile, err := fp.copySourceFile()
	if err != nil {
		return err
	}

	command, err := fp.buildCommand(commandTemplate, tempFile)
	if err != nil {
		return err
	}

	if err := fp.executeCommand(ctx, command); err != nil {
		return err
	}

	return nil
}

func (fp *fileProcessor) setupWorkDirectories() error {
	fp.cleanWorkDir()

	var err error
	fp.tempWorkDir, err = os.MkdirTemp("", "processing-*")
	if err != nil {
		return fmt.Errorf("unable to create temp folder: %w", err)
	}

	fp.tempWorkDirSrc = filepath.Join(fp.tempWorkDir, "src")
	if err = os.Mkdir(fp.tempWorkDirSrc, 0o700); err != nil {
		return fmt.Errorf("unable to create temp src folder: %w", err)
	}

	fp.tempWorkDirDst = filepath.Join(fp.tempWorkDir, "dst")
	if err = os.Mkdir(fp.tempWorkDirDst, 0o700); err != nil {
		return fmt.Errorf("unable to create temp dst folder: %w", err)
	}

	return nil
}

func (fp *fileProcessor) copySourceFile() (*os.File, error) {
	tempFile, err := os.CreateTemp(fp.tempWorkDirSrc, "file-*"+fp.originalExtension)
	if err != nil {
		return nil, fmt.Errorf("unable to create temp file: %w", err)
	}
	defer tempFile.Close()

	if _, err = fp.originalFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("unable to seek beginning of temp file: %w", err)
	}

	if _, err = io.Copy(tempFile, fp.originalFile); err != nil {
		return nil, fmt.Errorf("unable to write temp file: %w", err)
	}

	return tempFile, nil
}

func (fp *fileProcessor) buildCommand(commandTemplate *template.Template, tempFile *os.File) (string, error) {
	basename := filepath.Base(tempFile.Name())
	extension := filepath.Ext(basename)
	values := map[string]string{
		"src_folder": fp.tempWorkDirSrc,
		"dst_folder": fp.tempWorkDirDst,
		"name":       strings.TrimSuffix(basename, extension),
		"extension":  strings.TrimPrefix(extension, "."),
	}

	var cmdLine bytes.Buffer
	if err := commandTemplate.Execute(&cmdLine, values); err != nil {
		return "", fmt.Errorf("unable to generate command to be run: %w", err)
	}

	return cmdLine.String(), nil
}

func (fp *fileProcessor) executeCommand(ctx context.Context, command string) error {
	fp.logf("running: %s", command)

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if fp.configDir != "" {
		cmd.Dir = fp.configDir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w while running command:\n%s\nOutput:\n%s", err, command, string(output))
	}

	return nil
}

func (fp *fileProcessor) processResults() (*entity.ProcessResult, error) {
	files, err := os.ReadDir(fp.tempWorkDirDst)
	if err != nil {
		return nil, fmt.Errorf("unable to read temp directory: %w", err)
	}

	if len(files) != 1 {
		var filenames []string
		for _, f := range files {
			filenames = append(filenames, f.Name())
		}
		return nil, fmt.Errorf("unexpected number of files in output directory: expected 1, found %d (%s)", len(files), strings.Join(filenames, ", "))
	}

	processedFileName := files[0].Name()
	processedFilePath := filepath.Join(fp.tempWorkDirDst, processedFileName)

	processedExtension := strings.ToLower(filepath.Ext(processedFileName))

	stat, err := os.Stat(processedFilePath)
	if err != nil {
		return nil, fmt.Errorf("unable to get file size: %w", err)
	}
	processedSize := stat.Size()

	processedFilename := utils.TrimSuffixCaseInsensitive(fp.originalFilename, fp.originalExtension) + processedExtension

	return &entity.ProcessResult{
		ProcessedFilePath: processedFilePath,
		ProcessedFilename: processedFilename,
		OriginalSize:      fp.originalSize,
		ProcessedSize:     processedSize,
	}, nil
}
