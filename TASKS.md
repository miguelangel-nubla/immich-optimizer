# Configuration File

The YAML configuration file contains a list of tasks that are executed in order based on the file extension of the original file.

## How to Use

1. **Task Execution**: Tasks are executed sequentially if the file extension matches the original file.
2. **Refusing Uploads**: If no match is found for the file extension, the upload is refused.
3. **Keeping Extensions**: To keep files with a particular extension unchanged, leave the command as an empty string.
4. **Fallback Execution**: If multiple tasks match the same extension, they are executed in sequence as fallbacks. The process continues until a task completes successfully or all tasks fail, in which case the upload is blocked.

## Configuration Structure

The configuration file follows this structure:

```yaml
tasks:
  - name: taskA
    command: <command> {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}-new.ext && rm {{.folder}}/{{.name}}.{{.extension}}
    extensions:
      - jpeg
  - name: taskB
    command: <command 2> {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}-new.ext && rm {{.folder}}/{{.name}}.{{.extension}}
    extensions:
      - png
```

## Example

Here is an example entry of a task from the configuration file:

```yaml
  - name: jpeg-xl
    command: cjxl --lossless_jpeg=1 {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}-new.jxl && rm {{.folder}}/{{.name}}.{{.extension}}
    extensions:
      - jpeg
      - jpg
```

In this example, the task is executed for files with the `.jpeg` or `.jpg` extension.

- `extensions`: The file extensions to match.
- `command`: The command to execute for that task.

## Docker

If you are running on docker remember to mount a folder with the custom tasks.yaml folder inside the container in order to be able to load it

```yaml
services:
  immich-upload-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-upload-optimizer:latest
    ports:
      - "2283:2283"
    volumes:
      - <full path to folder with the config>:/etc/immich-upload-optimizer/config
    environment:
      - IUO_UPSTREAM=http://immich-server:2283
    depends_on:
      - immich-server
```

## Notes

- Ensure that the file extensions and commands are correctly specified.
- Tasks are executed in the order they are listed in the configuration file.
- Long-running tasks, such as video transcoding, may exceed the client timeout for HTTP requests. This utility attempts to mitigate this by sending periodic HTTP redirects to the client, but it may not be sufficient. However, tasks will continue running in the background, and the result will be uploaded to Immich upstream regardless of client disconnection.