# Configuration File

The yaml configuration file contains a list of tasks that are executed in order based on the file extension of the original file.

## How to Use

1. **Task Execution**: The tasks are tried sequentially as long as the file extension matches with the original file.
2. **Refusing Uploads**: If no match is found for the file extension, the upload will be refused.
3. **Keeping Extensions**: If you want to keep a particular extension as is, just leave the command as an empty string.
4. **Fallback Execution**: If multiple tasks match the same extension, they will be executed in sequence as fallbacks. The process continues until a task successfully completes or all tasks fail, in which case the upload will be blocked.

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

In this example, the task will be executed for files with the `.jpeg` or `.jpg` extension.

- `extensions`: The file extension to match.
- `command`: The command to execute for that task.

## Notes

- Ensure that the file extensions and commands are correctly specified.
- The tasks will be executed in the order they are listed in the configuration file.