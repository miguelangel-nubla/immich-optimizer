# Configuration File

The YAML configuration file defines a list of tasks executed sequentially based on the file extension of the uploaded file.

## Usage

1. **Task Execution**: Tasks run in order when the file extension matches.
2. **Upload Refusal**: If no matching extension is found, the upload is rejected.
3. **Preserving Extensions**: To leave files unchanged, set the command to an empty string.
4. **Fallback Execution**: When multiple tasks match an extension, they execute in sequence. The process stops when a task completes successfully. If all tasks fail, the upload is blocked.

## Configuration Structure

The configuration file follows this format:

```yaml
tasks:
  - name: taskA
    command: <command> {{.src_folder}}/{{.name}}.{{.extension}} {{.src_folder}}/{{.name}}.ext
    extensions:
      - jpeg
  - name: taskB
    command: <command 2> {{.src_folder}}/{{.name}}.{{.extension}} {{.src_folder}}/{{.name}}.ext
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

- `{{.src_folder}}`: Temporary working directory.
- `{{.name}}`: Filename without extension.
- `{{.extension}}`: File extension.

## Process Overview

When a file is uploaded, IUO:

1. Creates a temporary folder, e.g., `/tmp/processing-3398346076`.
2. Saves the file with a unique name, e.g., `file-2612480203.jpg`.
3. Executes the configured task command:

   ```sh
   cjxl --lossless_jpeg=1 {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.jxl
   ```

   This translates to:

   ```sh
   cjxl --lossless_jpeg=1 /tmp/processing-3398346076/file-2612480203.jpg /tmp/processing-3398346076/file-2612480203.jxl && rm /tmp/processing-3398346076/file-2612480203.jpg
   ```

4. If successful, IUO replaces the original file with the processed one and uploads it to Immich.

## Docker Setup

If using Docker, remember to mount a folder containing the `tasks.yaml` configuration file inside the container in order to be able to load it:

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    ports:
      - "2283:2283"
    volumes:
      - <full path to config folder>:/etc/immich-optimizer/config
    environment:
      - IUO_UPSTREAM=http://immich-server:2283
    depends_on:
      - immich-server
```

## Additional Notes

- Ensure file extensions and commands are correctly specified.
- Tasks execute in the order they appear in the configuration file.
- Long-running tasks (e.g., video transcoding) may exceed HTTP timeouts. IUO attempts to mitigate this by sending periodic HTTP redirects, but tasks will continue in the background even if the client disconnects. The processed file will still be uploaded to Immich regardless of client disconnection.

