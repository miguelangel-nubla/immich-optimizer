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
	// A result kept from an earlier task (see process) has its own folder.
	if own := res.Cleanup; own != nil {
		res.Cleanup = func() { own(); fp.cleanWorkDir() }
	} else {
		res.Cleanup = fp.cleanWorkDir
	}
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
	// gainMapDropped is true when the current task got the photo without its
	// gain map.
	gainMapDropped bool
	// hasGainMap caches whether the original is an HDR photo (nil: not
	// looked at yet).
	hasGainMap *bool
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
	download := entity.TaskInfoFrom(ctx).Source == entity.SourceDownload
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
	// kept is the result of a task that did not make the file smaller, kept
	// while the tasks after it try: if none does better, it is the answer
	// (the original is then delivered), not a failure.
	var kept *entity.ProcessResult
	matched, skipped := false, 0

	for i, task := range tasks {
		if !fp.takes(task) {
			if fp.matches(task) {
				matched = true
				skipped++
				fp.logf("task %s is not used for %s: it is an HDR photo (gain_maps: skip)", task.Name, fp.originalFilename)
			}
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

		convErr := fp.run(ctx, cmdTmpl, task, &attempt)
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
		// A command whose result is not smaller: a later task may do better.
		// Until one does, the first such result stands. A task without a
		// command (upload as it is) is always the answer. Download tasks are
		// for compatibility, not size: the first that works is used.
		if !download && strings.TrimSpace(task.Command) != "" && result.ProcessedSize >= result.OriginalSize && (kept != nil || fp.anyTakes(tasks[i+1:])) {
			if kept == nil {
				if kept, err = fp.keepAside(result); err != nil {
					return nil, err
				}
			}
			result = nil
			fp.cleanWorkDir()
			if fp.anyTakes(tasks[i+1:]) {
				fp.logf("task %s did not make %s smaller: trying the next task", task.Name, fp.originalFilename)
				continue
			}
		}
		break
	}

	if !matched {
		return nil, &entity.ProcessError{Err: fmt.Errorf("%w: %s", entity.ErrNoTask, fp.originalExtension)}
	}
	if result == nil && kept != nil {
		return kept, nil
	}
	if kept != nil {
		kept.Cleanup()
	}
	if result == nil && len(attempts) == 0 {
		return nil, &entity.ProcessError{Err: fmt.Errorf("every task for %s is set not to take HDR photos (gain_maps: skip, %d task(s))", fp.originalExtension, skipped)}
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

// matches reports whether the task is for the file's type.
func (fp *fileProcessor) matches(task entity.Task) bool {
	return slices.Contains(task.Extensions, utils.NormalizeExtension(fp.originalExtension))
}

// takes reports whether the task is used for the file: it is for its type,
// and does not skip it for being an HDR photo.
func (fp *fileProcessor) takes(task entity.Task) bool {
	if !fp.matches(task) {
		return false
	}
	if task.GainMaps != entity.GainMapsSkip {
		return true
	}
	if fp.hasGainMap == nil {
		has := motionphoto.HasGainMap(fp.originalFile.Name())
		fp.hasGainMap = &has
	}
	return !*fp.hasGainMap
}

func (fp *fileProcessor) anyTakes(tasks []entity.Task) bool {
	return slices.ContainsFunc(tasks, fp.takes)
}

// keepAside moves a result out of the work folder, which the next task
// starts afresh, into a folder of its own.
func (fp *fileProcessor) keepAside(res *entity.ProcessResult) (*entity.ProcessResult, error) {
	dir, err := os.MkdirTemp("", "processing-kept-*")
	if err != nil {
		return nil, fmt.Errorf("unable to create temp folder: %w", err)
	}
	path := filepath.Join(dir, filepath.Base(res.ProcessedFilePath))
	if err := os.Rename(res.ProcessedFilePath, path); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("unable to keep the result of task %s: %w", res.Task, err)
	}
	kept := *res
	kept.ProcessedFilePath = path
	kept.Cleanup = func() { os.RemoveAll(dir) }
	return &kept, nil
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

func (fp *fileProcessor) run(ctx context.Context, commandTemplate *template.Template, task entity.Task, attempt *entity.TaskAttempt) error {
	if err := fp.setupWorkDirectories(); err != nil {
		return err
	}
	motionPhotos := task.MotionPhotos

	tempFile, err := fp.motionPhotoSource(ctx, motionPhotos)
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
	fp.gainMapDropped = false
	if task.GainMaps == entity.GainMapsDrop {
		stripped, err := fp.gainMapSource(ctx, tempFile)
		if err != nil {
			return err
		}
		if stripped != nil {
			tempFile, fp.gainMapDropped = stripped, true
		}
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

// motionPhotoExtensions are the photo types motion photos come in.
var motionPhotoExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".heic": true, ".heif": true, ".hif": true, ".avif": true}

// motionPhotoSource prepares the task input for a motion photo according to
// the task's setting: only the photo (drop_video) or only the video
// (drop_photo). It returns nil when the regular copy must be used: the
// setting is "keep", or the file is not a motion photo.
func (fp *fileProcessor) motionPhotoSource(ctx context.Context, setting string) (*os.File, error) {
	if setting != entity.MotionPhotosDropVideo && setting != entity.MotionPhotosDropPhoto {
		return nil, nil
	}
	if !motionPhotoExtensions[fp.originalExtension] {
		return nil, nil
	}

	ext, split, what := fp.originalExtension, motionphoto.Strip, "photo"
	if setting == entity.MotionPhotosDropPhoto {
		ext, split, what = ".mp4", motionphoto.ExtractVideo, "video"
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

	info, err := split(ctx, fp.originalFile.Name(), tempFile.Name())
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

// gainMapSource returns the task input without its HDR gain map, or nil when
// the input has none (or is not a JPEG). A motion photo that still has its
// video (motion_photos: keep) keeps its gain map too: what follows the photo
// would go with it.
func (fp *fileProcessor) gainMapSource(ctx context.Context, input *os.File) (*os.File, error) {
	ext := strings.ToLower(filepath.Ext(input.Name()))
	if (ext != ".jpg" && ext != ".jpeg") || !motionphoto.HasGainMap(input.Name()) {
		return nil, nil
	}
	if motionphoto.IsMotionPhoto(input.Name()) {
		fp.logf("HDR photo kept with its gain map: it still has its motion video (motion_photos: keep)")
		return nil, nil
	}
	out, err := os.CreateTemp(fp.tempWorkDirSrc, "file-*"+ext)
	if err != nil {
		return nil, fmt.Errorf("unable to create temp file: %w", err)
	}
	_ = out.Close()
	if err := os.Remove(out.Name()); err != nil {
		return nil, fmt.Errorf("unable to prepare temp file: %w", err)
	}
	info, err := motionphoto.StripGainMap(ctx, input.Name(), out.Name())
	if err != nil {
		return nil, fmt.Errorf("unable to remove the gain map: %w", err)
	}
	// The task sees only the photo it gets.
	_ = os.Remove(input.Name())
	fp.logf("HDR photo (gain_maps: drop): passing only the ordinary photo to the task (%s)", utils.HumanReadableSize(info.StillBytes))
	return out, nil
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

// outputDrainTimeout is how long the rest of a finished command's output is
// waited for.
const outputDrainTimeout = 2 * time.Second

const (
	// outputBufferSize bounds the memory kept for a task's combined output.
	outputBufferSize = 64 << 10
	// OutputTailSize is how much trailing output is kept per attempt.
	OutputTailSize = 4 << 10
)

func (fp *fileProcessor) executeCommand(ctx context.Context, command string, attempt *entity.TaskAttempt) error {
	fp.logf("running: %s", command)

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(utils.EnvWithoutSecrets(os.Environ()), fp.env...)
	if fp.configDir != "" {
		cmd.Dir = fp.configDir
	}
	configureCmd(cmd)
	out := newTailBuffer(outputBufferSize)
	// The output goes through a pipe of our own, not one exec manages: Wait
	// then returns when the command itself exits, even if something it
	// started in the background still holds the pipe open.
	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("creating the output pipe: %w", err)
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(out, pr)
	}()
	if err = cmd.Start(); err == nil {
		_ = pw.Close()
		err = cmd.Wait()
		// Whatever the command left running in the background is stopped
		// before its output is looked at: it could still be writing there.
		killGroup(cmd)
	} else {
		_ = pw.Close()
	}
	select {
	case <-copied:
	case <-time.After(outputDrainTimeout):
		// A process that left the group still holds the pipe.
	}
	_ = pr.Close()
	<-copied
	attempt.OutputTail = out.Tail(OutputTailSize)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			attempt.ExitCode = exitErr.ExitCode()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if cause := context.Cause(ctx); cause != nil {
				return fmt.Errorf("command killed (%w): %w", cause, err)
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
		GainMapDropped:    fp.gainMapDropped,
	}, nil
}
