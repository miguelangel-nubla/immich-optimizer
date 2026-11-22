package processor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/adapter/motionphoto"
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

	// motionPhotos is the current task's motion photo setting.
	motionPhotos string
	// motionSplit is that setting when the input really was split for the
	// current task, "" otherwise.
	motionSplit string
	// taskName is the task that succeeded, taskCommand its command template.
	taskName    string
	taskCommand string
	// env is what the current task's command gets in its environment.
	env []string
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

	var attempts []entity.TaskAttempt
	var result *entity.ProcessResult
	matched := false

	for _, task := range tasks {
		if !slices.Contains(task.Extensions, utils.NormalizeExtension(fp.originalExtension)) {
			continue
		}
		matched = true

		start := time.Now()
		attempt := entity.TaskAttempt{Task: task.Name, ExitCode: -1}

		cmdTmpl, tmplErr := template.New("command").Option("missingkey=error").Parse(task.Command)
		if tmplErr != nil {
			attempt.Err = fmt.Sprintf("unable to parse command template: %v", tmplErr)
			attempt.Duration = time.Since(start)
			attempts = append(attempts, attempt)
			continue
		}

		convErr := fp.run(ctx, cmdTmpl, task.MotionPhotos, &attempt)
		if convErr == nil {
			// A command that reports success without leaving exactly one
			// file with content has failed like any other: the next task
			// gets its turn.
			attempt.ExitCode = 0
			fp.taskName, fp.taskCommand = task.Name, task.Command
			result, convErr = fp.processResults()
		}
		attempt.Duration = time.Since(start)
		if convErr != nil {
			attempt.Err = convErr.Error()
			attempts = append(attempts, attempt)
			fp.logf("task %s failed for %s: %v", task.Name, fp.originalFilename, convErr)
			fp.cleanWorkDir()
			if ctx.Err() != nil {
				// No point trying further tasks once cancelled.
				break
			}
			continue
		}
		attempts = append(attempts, attempt)
		break
	}

	if !matched {
		return nil, &entity.ProcessError{Err: fmt.Errorf("%w: %s", entity.ErrNoTask, fp.originalExtension)}
	}
	if result == nil {
		last := attempts[len(attempts)-1]
		return nil, &entity.ProcessError{
			Attempts: attempts,
			Err:      fmt.Errorf("all %d matching task(s) failed (last: %s: %s)", len(attempts), last.Task, firstLine(last.Err)),
		}
	}

	return result, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
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

func (fp *fileProcessor) run(ctx context.Context, commandTemplate *template.Template, motionPhotos string, attempt *entity.TaskAttempt) error {
	if err := fp.setupWorkDirectories(); err != nil {
		return err
	}

	tempFile, err := fp.motionPhotoSource(motionPhotos)
	if err != nil {
		return err
	}
	split := tempFile != nil
	if tempFile == nil {
		if tempFile, err = fp.copySourceFile(); err != nil {
			return err
		}
	}
	fp.motionPhotos = motionPhotos
	fp.motionSplit = ""
	if split {
		fp.motionSplit = motionPhotos
	}

	command, err := fp.buildCommand(commandTemplate, tempFile)
	if err != nil {
		return err
	}
	attempt.Command = command
	fp.env = fp.commandEnv(ctx, tempFile, attempt.Task)

	if strings.TrimSpace(command) == "" {
		return fp.copyPassthroughResult(tempFile)
	}

	return fp.executeCommand(ctx, command, attempt)
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

	if _, err = fp.originalFile.Seek(0, io.SeekStart); err == nil {
		_, err = io.Copy(tempFile, fp.originalFile)
	}
	// A failed close can mean the copy is not complete on disk.
	if cerr := tempFile.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("unable to write temp file: %w", err)
	}

	return tempFile, nil
}

// motionPhotoSource prepares the task input for a JPEG motion photo according
// to the task's setting: only the photo (drop_video) or only the video
// (drop_photo). It returns nil when the regular copy must be used: the
// setting is "keep", the file is not a JPEG, or it is not a motion photo.
func (fp *fileProcessor) motionPhotoSource(setting string) (*os.File, error) {
	if setting != entity.MotionPhotosDropVideo && setting != entity.MotionPhotosDropPhoto {
		return nil, nil
	}
	if fp.originalExtension != ".jpg" && fp.originalExtension != ".jpeg" {
		return nil, nil
	}

	ext, split, what := fp.originalExtension, motionphoto.StripJPEG, "photo"
	if setting == entity.MotionPhotosDropPhoto {
		ext, split, what = ".mp4", motionphoto.ExtractVideoJPEG, "video"
	}
	// Reserve a unique name, then let the splitter create the file itself.
	tempFile, err := os.CreateTemp(fp.tempWorkDirSrc, "file-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("unable to create temp file: %w", err)
	}
	_ = tempFile.Close()
	if err := os.Remove(tempFile.Name()); err != nil {
		return nil, fmt.Errorf("unable to prepare temp file: %w", err)
	}

	info, err := split(fp.originalFile.Name(), tempFile.Name())
	if errors.Is(err, motionphoto.ErrNotMotionPhoto) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("unable to split motion photo: %w", err)
	}
	// No file name here: the processor only knows a temporary one. The
	// engine's result line names the file and the setting.
	fp.logf("motion photo (%s): passing only the %s to the task (photo %s, video %s)",
		setting, what,
		utils.HumanReadableSize(info.StillBytes), utils.HumanReadableSize(info.VideoBytes))
	return tempFile, nil
}

func (fp *fileProcessor) copyPassthroughResult(tempFile *os.File) error {
	srcPath := tempFile.Name()
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("unable to open passthrough source file: %w", err)
	}
	defer src.Close()

	dstPath := filepath.Join(fp.tempWorkDirDst, filepath.Base(srcPath))
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("unable to create passthrough output file: %w", err)
	}

	_, err = io.Copy(dst, src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("unable to copy passthrough output file: %w", err)
	}

	return nil
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

// commandEnv is what a task command finds in its environment, next to what
// the optimizer itself was started with: everything the command's template
// can use, under the same names, and what a template must not be given.
//
// The file's own name is only here. A name is whatever the uploader made it,
// and a template value ends up in a shell command line; a variable is read
// by the command as data ("$IO_ORIGINAL_NAME", quoted).
func (fp *fileProcessor) commandEnv(ctx context.Context, tempFile *os.File, task string) []string {
	basename := filepath.Base(tempFile.Name())
	extension := filepath.Ext(basename)
	info := entity.TaskInfoFrom(ctx)
	if info.OriginalName == "" {
		info.OriginalName = fp.originalFilename
	}
	env := []string{
		"IO_SRC_FOLDER=" + fp.tempWorkDirSrc,
		"IO_DST_FOLDER=" + fp.tempWorkDirDst,
		"IO_NAME=" + strings.TrimSuffix(basename, extension),
		"IO_EXTENSION=" + strings.TrimPrefix(extension, "."),
		"IO_ORIGINAL_NAME=" + strings.NewReplacer("\x00", "", "\n", " ", "\r", " ").Replace(info.OriginalName),
		"IO_SOURCE=" + info.Source,
		"IO_TASK=" + task,
	}
	if fp.motionPhotos != "" {
		// Lets scripts apply the task's motion photo setting to formats the
		// optimizer does not split itself (HEIC).
		env = append(env, "IO_MOTION_PHOTOS="+fp.motionPhotos)
	}
	return env
}

const (
	// outputBufferSize bounds the memory kept for a task's combined output.
	outputBufferSize = 64 << 10
	// OutputTailSize is how much trailing output is kept per attempt.
	OutputTailSize = 4 << 10
)

func (fp *fileProcessor) executeCommand(ctx context.Context, command string, attempt *entity.TaskAttempt) error {
	fp.logf("running: %s", command)

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(), fp.env...)
	if fp.configDir != "" {
		cmd.Dir = fp.configDir
	}
	configureCmd(cmd)
	out := newTailBuffer(outputBufferSize)
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	attempt.OutputTail = out.Tail(OutputTailSize)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			attempt.ExitCode = exitErr.ExitCode()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if cause := context.Cause(ctx); cause != nil {
				return fmt.Errorf("command killed (%v): %w", cause, err)
			}
		}
		return fmt.Errorf("%w while running command", err)
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
	// An empty original is reported as such by the engine, not as a task
	// that produced nothing.
	if processedSize == 0 && fp.originalSize > 0 {
		return nil, errors.New("the output file is empty")
	}

	processedFilename := utils.TrimSuffixCaseInsensitive(fp.originalFilename, fp.originalExtension) + processedExtension

	return &entity.ProcessResult{
		ProcessedFilePath: processedFilePath,
		ProcessedFilename: processedFilename,
		OriginalSize:      fp.originalSize,
		ProcessedSize:     processedSize,
		Task:              fp.taskName,
		TaskCommand:       fp.taskCommand,
		MotionPhotos:      fp.motionSplit,
	}, nil
}
