package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

type TaskProcessor struct {
	OriginalFilename  string
	OriginalFile      *os.File
	OriginalExtension string
	OriginalSize      int64

	tempFileOriginalFile string

	ProcessedFilename  string
	ProcessedFile      *os.File
	ProcessedExtension string
	ProcessedSize      int64

	tempWorkDir    string
	tempWorkDirSrc string
	tempWorkDirDst string

	logger    *customLogger
	semaphore chan struct{}
	configDir string
}

func NewTaskProcessor(filename string) (tp *TaskProcessor, err error) {
	originalFile, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("unable to open file: %w", err)
	}

	stat, err := originalFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("unable to get file info: %w", err)
	}

	originalSize := stat.Size()
	originalExtension := strings.ToLower(path.Ext(filename))

	tp = &TaskProcessor{
		OriginalFilename:  filepath.Base(filename),
		OriginalFile:      originalFile,
		OriginalExtension: originalExtension,
		OriginalSize:      originalSize,
	}

	return
}

func (tp *TaskProcessor) SetLogger(logger *customLogger) {
	tp.logger = logger
}

func (tp *TaskProcessor) SetSemaphore(semaphore chan struct{}) {
	tp.semaphore = semaphore
}

func (tp *TaskProcessor) SetConfigDir(configDir string) {
	tp.configDir = configDir
}

func (tp *TaskProcessor) logf(str string, args ...any) {
	if tp.logger != nil {
		tp.logger.Printf(str, args...)
	}
}

func (tp *TaskProcessor) Process(tasks []Task) (err error) {
	err = fmt.Errorf("no task found for file extension %s", tp.OriginalExtension)
	var errors []error

	for _, task := range tasks {
		if !slices.Contains(task.Extensions, normalizeExtension(tp.OriginalExtension)) {
			continue
		}

		convErr := tp.run(task.CommandTemplate)
		if convErr != nil {
			errors = append(errors, fmt.Errorf("\ntask %s failed: %w", task.Name, convErr))
			tp.cleanWorkDir()
			continue
		}
		err = nil
		break
	}

	if len(errors) > 1 {
		err = fmt.Errorf("errors: %v", errors)
	} else if len(errors) == 1 {
		err = errors[0]
	}

	return
}

func (tp *TaskProcessor) Close() (err error) {
	tp.cleanWorkDir()

	err = tp.OriginalFile.Close()
	if err != nil {
		tp.logf("unable to close original file: %v", err)
	}

	if tp.tempFileOriginalFile != "" {
		err = os.Remove(tp.tempFileOriginalFile)
		if err != nil {
			tp.logf("unable to remove temp file: %v", err)
		}
	}

	return
}

func (tp *TaskProcessor) cleanWorkDir() (err error) {
	if tp.tempWorkDir != "" {
		err = os.RemoveAll(tp.tempWorkDir)
		if err != nil {
			tp.logf("unable to clean temp folder: %v", err)
		}
	}

	tp.tempWorkDir = ""
	tp.tempWorkDirSrc = ""
	tp.tempWorkDirDst = ""

	return
}

func (tp *TaskProcessor) run(commandTemplate *template.Template) error {
	if err := tp.setupWorkDirectories(); err != nil {
		return err
	}

	tempFile, err := tp.copySourceFile()
	if err != nil {
		return err
	}

	command, err := tp.buildCommand(commandTemplate, tempFile)
	if err != nil {
		return err
	}

	if err := tp.executeCommand(command); err != nil {
		return err
	}

	return tp.processResults()
}

func (tp *TaskProcessor) setupWorkDirectories() error {
	tp.cleanWorkDir()

	var err error
	tp.tempWorkDir, err = os.MkdirTemp("", "processing-*")
	if err != nil {
		return fmt.Errorf("unable to create temp folder: %w", err)
	}

	tp.tempWorkDirSrc = path.Join(tp.tempWorkDir, "src")
	if err = os.Mkdir(tp.tempWorkDirSrc, 0o700); err != nil {
		return fmt.Errorf("unable to create temp src folder: %w", err)
	}

	tp.tempWorkDirDst = path.Join(tp.tempWorkDir, "dst")
	if err = os.Mkdir(tp.tempWorkDirDst, 0o700); err != nil {
		return fmt.Errorf("unable to create temp dst folder: %w", err)
	}

	return nil
}

func (tp *TaskProcessor) copySourceFile() (*os.File, error) {
	tempFile, err := os.CreateTemp(tp.tempWorkDirSrc, "file-*"+tp.OriginalExtension)
	if err != nil {
		return nil, fmt.Errorf("unable to create temp file: %w", err)
	}

	if _, err = tp.OriginalFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("unable to seek beginning of temp file: %w", err)
	}

	if _, err = io.Copy(tempFile, tp.OriginalFile); err != nil {
		return nil, fmt.Errorf("unable to write temp file: %w", err)
	}
	tempFile.Close()

	return tempFile, nil
}

func (tp *TaskProcessor) buildCommand(commandTemplate *template.Template, tempFile *os.File) (string, error) {
	basename := path.Base(tempFile.Name())
	extension := path.Ext(basename)
	values := map[string]string{
		"src_folder": tp.tempWorkDirSrc,
		"dst_folder": tp.tempWorkDirDst,
		"name":       strings.TrimSuffix(basename, extension),
		"extension":  strings.TrimPrefix(extension, "."),
	}

	var cmdLine bytes.Buffer
	if err := commandTemplate.Execute(&cmdLine, values); err != nil {
		return "", fmt.Errorf("unable to generate command to be run: %w", err)
	}

	return cmdLine.String(), nil
}

func (tp *TaskProcessor) executeCommand(command string) error {
	// Limit the number of concurrent tasks running
	if tp.semaphore != nil {
		tp.semaphore <- struct{}{}
		defer func() { <-tp.semaphore }()
	}

	tp.logf("running: %s", command)

	cmd := exec.Command("sh", "-c", command)
	if tp.configDir != "" {
		cmd.Dir = tp.configDir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w while running command:\n%s\nOutput:\n%s", err, command, string(output))
	}

	return nil
}

func (tp *TaskProcessor) processResults() error {
	files, err := os.ReadDir(tp.tempWorkDirDst)
	if err != nil {
		return fmt.Errorf("unable to read temp directory: %w", err)
	}

	if len(files) != 1 {
		return fmt.Errorf("unexpected number of files in temp directory: %d", len(files))
	}

	processedFileName := files[0].Name()
	processedFile := path.Join(tp.tempWorkDirDst, processedFileName)

	tp.ProcessedFile, err = os.Open(processedFile)
	if err != nil {
		return fmt.Errorf("unable to open temp file: %w", err)
	}

	tp.ProcessedExtension = strings.ToLower(path.Ext(processedFileName))

	stat, err := os.Stat(processedFile)
	if err != nil {
		return fmt.Errorf("unable to get file size: %w", err)
	}
	tp.ProcessedSize = stat.Size()

	tp.ProcessedFilename = trimSuffixCaseInsensitive(tp.OriginalFilename, tp.OriginalExtension) + tp.ProcessedExtension

	return nil
}

func (tp *TaskProcessor) GetProcessedFilePath() (string, error) {
	if tp.ProcessedFile == nil {
		return "", fmt.Errorf("no processed file available")
	}
	return tp.ProcessedFile.Name(), nil
}
