package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"text/template"
)

func processTasks(originalFile io.Reader, originalExtension string, logger *customLogger) (processedFile io.Reader, processedExtension string, processedSize int64, err error) {
	// Limit the number of concurrent tasks
	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	err = fmt.Errorf("no task found for file extension %s", originalExtension)
	var errors []error

	checkExt := strings.TrimPrefix(originalExtension, ".")
	for _, task := range config.Tasks {
		if !slices.Contains(task.Extensions, checkExt) {
			continue
		}

		var convErr error
		processedFile, processedExtension, processedSize, convErr = processFile(task.CommandTemplate, originalFile, originalExtension, logger)
		if convErr != nil {
			errors = append(errors, fmt.Errorf("task %s failed: %w", task.Name, convErr))
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

func processFile(commandTemplate *template.Template, originalFile io.Reader, originalExtension string, logger *customLogger) (processedFile io.Reader, processedExtension string, processedSize int64, err error) {
	tempDir, err := os.MkdirTemp("", "processing-*")
	if err != nil {
		err = fmt.Errorf("unable to create temp folder: %w", err)
		return
	}
	//defer os.RemoveAll(tempDir)

	tempFile, err := os.CreateTemp(tempDir, "file-*"+originalExtension)
	if err != nil {
		err = fmt.Errorf("unable to create temp file: %w", err)
		return
	}

	_, err = io.Copy(tempFile, originalFile)
	if err != nil {
		err = fmt.Errorf("unable to write temp file: %w", err)
		return
	}
	tempFile.Close()

	basename := path.Base(tempFile.Name())
	extension := path.Ext(basename)
	values := map[string]string{
		"folder":    tempDir,
		"name":      strings.TrimSuffix(basename, extension),
		"extension": strings.TrimPrefix(extension, "."),
	}
	var cmdLine bytes.Buffer
	err = commandTemplate.Execute(&cmdLine, values)
	if err != nil {
		err = fmt.Errorf("unable to generate command to be run: %w", err)
		return
	}

	logger.Printf("running: %s", cmdLine.String())

	cmd := exec.Command("sh", "-c", cmdLine.String())
	cmd.Dir = path.Dir(configFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%w while running command:\n%s\nOutput:\n%s", err, cmdLine.String(), string(output))
		return
	}

	files, err := os.ReadDir(tempDir)
	if err != nil {
		err = fmt.Errorf("unable to read temp directory: %w", err)
		return
	}

	if len(files) != 1 {
		err = fmt.Errorf("unexpected number of files in temp directory: %d", len(files))
		return
	}

	processedFile, err = os.Open(path.Join(tempDir, files[0].Name()))
	if err != nil {
		err = fmt.Errorf("unable to open temp file: %w", err)
		return
	}

	processedExtension = path.Ext(files[0].Name())

	stat, err := os.Stat(path.Join(tempDir, files[0].Name()))
	if err != nil {
		err = fmt.Errorf("unable to get file size: %w", err)
	}
	processedSize = stat.Size()

	return
}
