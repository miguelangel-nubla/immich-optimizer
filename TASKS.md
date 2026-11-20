# Configuration File

The YAML configuration file defines a list of tasks executed sequentially based on the file extension of the uploaded file.

## Usage

1. **Task Execution**: Tasks run in order when the file extension matches.
2. **Upload Refusal**: If no matching extension is found, the upload is refused and the file is saved to the undone folder (unless `IO_UNDONE_UNMATCHED=false`).
3. **Preserving Extensions**: To leave matching files unchanged, set the command to an empty string.
4. **Fallback Execution**: When multiple tasks match an extension, they execute in sequence. The process stops when a task completes successfully. If all tasks fail, the upload is blocked and the original is saved to the undone folder as `undone/<sha1>/original.file` (SHA-1 of the content, the same checksum Immich stores) with a `metadata.json` describing the failure. The original is never uploaded in its place.

## Configuration Structure

The configuration file follows this format:

```yaml
tasks:
  - name: taskA
    command: <command> {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.ext
    extensions:
      - jpeg
  - name: taskB
    command: <command 2> {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.ext
    extensions:
      - png
```

## Example Task

Below is an example task entry:

```yaml
  - name: jpeg-xl
    command: cjxl --lossless_jpeg=1 {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.jxl
    extensions:
      - jpeg
      - jpg
```

This task processes `.jpeg` and `.jpg` files.

- `extensions`: Specifies file extensions to match.
- `command`: Defines the processing command.

### Placeholder Variables

To ensure proper file handling, use these placeholders in your commands:

- `{{.src_folder}}`: Temporary source directory containing the staged input file.
- `{{.dst_folder}}`: Temporary output directory. Commands must write exactly one output file here, unless the command is empty.
- `{{.name}}`: Filename without extension.
- `{{.extension}}`: File extension.

## Process Overview

When a file is uploaded, the optimizer:

1. Creates temporary source and output folders, e.g., `/tmp/processing-3398346076/src` and `/tmp/processing-3398346076/dst`.
2. Saves the file with a unique name, e.g., `/tmp/processing-3398346076/src/file-2612480203.jpg`.
3. Executes the configured task command:

   ```sh
   cjxl --lossless_jpeg=1 {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.jxl
   ```

   This translates to:

   ```sh
   cjxl --lossless_jpeg=1 /tmp/processing-3398346076/src/file-2612480203.jpg /tmp/processing-3398346076/dst/file-2612480203.jxl
   ```

4. If successful and the output is smaller, the optimizer uploads the processed file to Immich under the original name with the new extension. If the output is not smaller, the original is uploaded unchanged. An empty output counts as a failure.

## Verify command

A task can have a `verify` command that proves its output is a faithful conversion of its input. It must exit with status 0 when the output is good. Library mode only replaces an existing asset when the task that converted it has a verify command and it succeeds; proxy and watcher mode do not use it.

```yaml
  - name: jpeg-xl
    command: cjxl --lossless_jpeg=1 -d 0 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
    verify: djxl "{{.output}}" "{{.tmp_folder}}/rebuilt.jpg" && cmp "{{.tmp_folder}}/rebuilt.jpg" "{{.src_folder}}/{{.name}}.{{.extension}}"
    extensions: [jpg, jpeg]
```

In a verify command, `{{.src_folder}}/{{.name}}.{{.extension}}` is the original, `{{.output}}` is the converted file and `{{.tmp_folder}}` is an empty scratch folder.

## Motion photos

`motion_photos` controls what a task command receives when the file is a motion photo (a photo with an embedded video): `keep` (the file as it is, the default), `drop_video` (only the photo) or `drop_photo` (only the video, as `.mp4`). Set it at the top level as the default and on a task to override it:

```yaml
motion_photos: drop_video
tasks:
  - name: taskA
    motion_photos: keep
    command: ...
```

Commands also get the task's value in the `IO_MOTION_PHOTOS` environment variable. See "Motion photos" in the README for the details.

## Docker Setup

If using Docker, remember to mount a folder containing the `tasks.yaml` configuration file inside the container in order to be able to load it:

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_IMMICH_API_KEY=your-api-key
    volumes:
      - /path/to/watch:/watch
      - /path/to/undone:/undone
      - <full path to config folder>:/etc/immich-optimizer/config
    depends_on:
      - immich-server
```

## Additional Notes

- Ensure file extensions and commands are correctly specified.
- Tasks execute in the order they appear in the configuration file.
- Jobs are not tied to the client connection: if the client disconnects or times out, conversion and upload to Immich continue, and a retry of the same file while it is running or within 30 minutes gets the same result instead of uploading twice. Only shutdown (after `IO_SHUTDOWN_TIMEOUT_SECONDS`) or `IO_TASK_TIMEOUT_SECONDS` stops a job. Browser uploads are kept alive with periodic redirects.
- `IO_TASK_TIMEOUT_SECONDS` (default `0`, no limit) bounds the processing of one file. When it expires, the task's whole process tree is killed (including tools such as HandBrakeCLI or ffmpeg started by the command) and the file is saved to undone with `"timed_out": true` on the attempt in its `metadata.json`.
- Commands run through `sh -c` in the directory of the tasks file. Their combined output is kept in memory only up to 64 KiB, and the last 4 KiB of each failed attempt is written to the undone `metadata.json`.
