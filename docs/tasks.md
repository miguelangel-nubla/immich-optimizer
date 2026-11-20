# Tasks

The tasks file tells the optimizer which command converts which file type. It is the same file for every mode. Set its path with [`IO_TASKS_FILE`](configuration.md#tasks); in the Docker image the default is `/etc/immich-optimizer/config/tasks.yaml`.

```yaml
motion_photos: drop_video          # optional, see "Motion photos"
library_filter:                    # optional, library mode only: which assets to convert
  takenAt: {lt: 2024-01-01T00:00:00Z}

tasks:
  - name: jpeg-xl
    command: cjxl --lossless_jpeg=1 -d 0 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
    extensions: [jpg, jpeg, png]

  - name: passthrough
    command: ""                    # an empty command uploads the file unchanged
    extensions: [heic, mp4, mov]
```

`library_filter` is only read by [library mode](library.md#choosing-what-to-convert), where it limits which existing assets are converted. Proxy and watcher mode ignore it.

## How a file is handled

1. The file's extension selects the tasks, in the order they appear in the file.
2. The first matching task runs. If its command fails, the next matching task is tried.
3. **If the output is smaller than the original**, the output is uploaded under the original name with the new extension. If it is not smaller, the original is uploaded unchanged.
4. **If every matching task fails**, or the output is empty, nothing is uploaded and the original is kept in the [undone folder](undone.md).
5. **If no task matches the extension**, the file is refused (and kept in the undone folder unless [`IO_UNDONE_UNMATCHED`](configuration.md#undone-folder-and-audit-log) is `false`). List every type you want to accept, with an empty command for those you do not convert.

## Writing a command

| Placeholder | Meaning |
|---|---|
| `{{.src_folder}}` | temporary folder that holds the input file |
| `{{.dst_folder}}` | temporary output folder. The command must leave **exactly one file** here |
| `{{.name}}` | the input's file name without extension (a temporary name, not the original one) |
| `{{.extension}}` | the input's extension, without the dot |

- Commands run through `sh -c`, in the folder of the tasks file, so scripts next to it can be called as `./script.sh`.
- Quote the paths.
- An unknown placeholder stops the optimizer at startup with an error.
- The output's extension is taken from the file the command wrote.
- The last 4 KiB of a failed command's output is saved with the file in the undone folder.
- A command that runs longer than [`IO_TASKS_TIMEOUT`](configuration.md#tasks) is killed together with everything it started.

## Bundled profiles

The image ships these under `/etc/immich-optimizer/bundled-configs/<profile>/`. The `lossless` profile is the default. To use another one, point `IO_TASKS_FILE` at it, or copy it and mount your own folder at `/etc/immich-optimizer/config`.

| Profile | Photos | Videos |
|---|---|---|
| [`lossless`](../config/lossless/tasks.yaml) (default) | JPEG and PNG to JPEG XL without any loss; the rest unchanged | unchanged |
| [`profile1`](../config/profile1/tasks.yaml) | lossy JPEG XL (quality 75), HEIC included | HandBrake |
| [`storage-saver-amd-gpu`](../config/storage-saver-amd-gpu/tasks.yaml) | lossy JPEG XL, at most 4096 px | HEVC through VAAPI (needs a [custom image](docker.md#custom-image)) |
| [`passthrough-all`](../config/passthrough-all/tasks.yaml) | unchanged | unchanged |

Every bundled profile accepts every file type Immich accepts.

## Motion photos

A motion photo is a photo with a short video embedded in the same file (Android/Pixel JPEGs, Samsung HEICs). The `motion_photos` key of your `tasks.yaml` controls **what each task command receives** for such a file:

| Value | The task receives |
|---|---|
| `keep` | the file as it is, photo and video together (built-in default) |
| `drop_video` | only the photo |
| `drop_photo` | only the video, as an `.mp4` |

Set it at the top of the file as the default for all tasks, and on a task to override it:

```yaml
motion_photos: drop_video        # default for every task below

tasks:
  - name: jpeg-xl                # gets only the photo
    command: cjxl --lossless_jpeg=1 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
    extensions: [jpg, jpeg]

  - name: untouched-fallback     # gets the whole file
    motion_photos: keep
    command: ""
    extensions: [jpg, jpeg]
```

- The bundled `lossless`, `profile1` and `storage-saver-amd-gpu` profiles set `motion_photos: drop_video`: **keep the photo, drop the video**. A tasks file without the key keeps motion photos as they are.
- The setting only affects motion photos. Every other file reaches the task unchanged.
- When several tasks match a file, each one gets its input prepared by its own setting.
- With `drop_video` the motion flag is also switched off in the photo's metadata, so Immich does not look for a video that is gone.
- With `drop_photo` the input is `{{.name}}.mp4`, so `{{.extension}}` is `mp4` in the command.
- The log says what happened: `Optimized IMG_1234.jpg (proxy, motion photo: drop_video): 5.2 MB -> 1.9 MB`.
- If all tasks fail, the **untouched original, video included**, is saved to the undone folder.

Things to know:

- **`keep` with a converting task rarely keeps the video.** Lossy encoders drop it without notice, and `cjxl --lossless_jpeg=1` hides it inside the output where Immich cannot play it. To really keep a motion photo intact, use `keep` on a passthrough task (empty command).
- **JPEG** motion photos (Google `MotionPhoto` and the older `MicroVideo` format) are split by the optimizer itself. A file is only split when an MP4 really starts where its metadata says; otherwise it is passed as it is.
- **HEIC** motion photos are not split by the optimizer. The task command gets the setting in the `IO_MOTION_PHOTOS` environment variable and applies it itself, as the bundled `profile1/heic2jxl.sh` does.
- Older Samsung JPEGs that store the video in a Samsung-specific trailer without the standard motion-photo metadata are not detected.
- Apple Live Photos are different: they are two separate files and both are processed normally.

## Downloads

A file stored in Immich may be in a format the person downloading it cannot open; JPEG XL is the usual case. The `downloads` list says how to turn such a file into something else **when its original is downloaded through the [proxy](proxy.md)**. It is written like `tasks`:

```yaml
downloads:
  - name: jxl-to-jpeg
    command: djxl "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jpg"
    extensions: [jxl]
```

- **Nothing in Immich changes.** The conversion is made for that one download and thrown away. Immich's own apps keep showing the stored file.
- **A JPEG comes back exactly as it was uploaded** when the JXL was made with `cjxl --lossless_jpeg=1`, as the bundled `lossless` profile does: `djxl` rebuilds the original file, byte for byte. A JXL made from a PNG, or with a lossy task, becomes a new JPEG.
- **Only the download of a single original is converted** ("Download" in the web and mobile apps, `GET /api/assets/{id}/original`). Archives of several assets and the edited version of an asset are sent as Immich makes them.
- **If the command fails, the stored file is sent**, and the log says so.
- The file is downloaded from Immich in full and converted on every request, also when only a part of it is asked for. It takes one more request to Immich per download, to learn the file's type.
- It is off unless your tasks file has the list. The bundled profiles carry it as a comment.
- Only proxy mode reads it.
