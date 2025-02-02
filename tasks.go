package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
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
	OriginalFile      io.Reader
	OriginalExtension string
	OriginalSize      int64

	ProcessedFilename  string
	ProcessedFile      io.Reader
	ProcessedExtension string
	ProcessedSize      int64

	tempDir string

	logger *customLogger
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

func NewTaskProcessorFromMultipart(file multipart.File, header *multipart.FileHeader) (tp *TaskProcessor, err error) {
	originalSize := header.Size
	originalExtension := strings.ToLower(path.Ext(header.Filename))

	tp = &TaskProcessor{
		OriginalFilename:  header.Filename,
		OriginalFile:      file,
		OriginalExtension: originalExtension,
		OriginalSize:      originalSize,
	}

	return
}

func (tp *TaskProcessor) SetLogger(logger *customLogger) {
	tp.logger = logger
}

func (tp *TaskProcessor) logf(str string, args ...interface{}) {
	if tp.logger != nil {
		tp.logger.Printf(str, args...)
	}
}

func (tp *TaskProcessor) Process(tasks []Task) (err error) {
	err = fmt.Errorf("no task found for file extension %s", tp.OriginalExtension)
	var errors []error

	checkExt := strings.TrimPrefix(tp.OriginalExtension, ".")
	for _, task := range tasks {
		if !slices.Contains(task.Extensions, checkExt) {
			continue
		}

		convErr := tp.run(task.CommandTemplate)
		if convErr != nil {
			errors = append(errors, fmt.Errorf("\ntask %s failed: %w", task.Name, convErr))
			tp.Clean()
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

func (tp *TaskProcessor) Clean() (err error) {
	if tp.tempDir != "" {
		err = os.RemoveAll(tp.tempDir)
		if err != nil {
			tp.logf("unable to clean temp folder: %v", err)
		}
	}

	tp.tempDir = ""

	return
}

func (tp *TaskProcessor) run(commandTemplate *template.Template) (err error) {
	tp.Clean()

	tp.tempDir, err = os.MkdirTemp("", "processing-*")
	if err != nil {
		err = fmt.Errorf("unable to create temp folder: %w", err)
		return
	}

	tempFile, err := os.CreateTemp(tp.tempDir, "file-*"+tp.OriginalExtension)
	if err != nil {
		err = fmt.Errorf("unable to create temp file: %w", err)
		return
	}

	_, err = io.Copy(tempFile, tp.OriginalFile)
	if err != nil {
		err = fmt.Errorf("unable to write temp file: %w", err)
		return
	}
	tempFile.Close()

	basename := path.Base(tempFile.Name())
	extension := path.Ext(basename)
	values := map[string]string{
		"folder":    tp.tempDir,
		"name":      strings.TrimSuffix(basename, extension),
		"extension": strings.TrimPrefix(extension, "."),
	}

	var cmdLine bytes.Buffer
	err = commandTemplate.Execute(&cmdLine, values)
	if err != nil {
		err = fmt.Errorf("unable to generate command to be run: %w", err)
		return
	}

	// Limit the number of concurrent tasks running
	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	tp.logf("running: %s", cmdLine.String())

	cmd := exec.Command("sh", "-c", cmdLine.String())
	cmd.Dir = path.Dir(configFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%w while running command:\n%s\nOutput:\n%s", err, cmdLine.String(), string(output))
		return
	}

	files, err := os.ReadDir(tp.tempDir)
	if err != nil {
		err = fmt.Errorf("unable to read temp directory: %w", err)
		return
	}

	if len(files) != 1 {
		err = fmt.Errorf("unexpected number of files in temp directory: %d", len(files))
		return
	}

	tp.ProcessedFile, err = os.Open(path.Join(tp.tempDir, files[0].Name()))
	if err != nil {
		err = fmt.Errorf("unable to open temp file: %w", err)
		return
	}

	tp.ProcessedExtension = strings.ToLower(path.Ext(files[0].Name()))

	stat, err := os.Stat(path.Join(tp.tempDir, files[0].Name()))
	if err != nil {
		err = fmt.Errorf("unable to get file size: %w", err)
	}
	tp.ProcessedSize = stat.Size()

	tp.ProcessedFilename = strings.TrimSuffix(tp.OriginalFilename, tp.OriginalExtension) + tp.ProcessedExtension

	return
}
