# Tasks

The tasks file tells the optimizer which command converts which file type. It is the same file for every mode. Set its path with [`IO_TASKS_FILE`](configuration.md#tasks); in the Docker image the default is `/etc/immich-optimizer/config/tasks.yaml`.

```yaml
motion_photos: drop_video          # optional, see "Motion photos"
gain_maps: keep                    # optional, see "HDR photos"

tasks:
  - name: jpeg-xl
    command: cjxl --lossless_jpeg=1 -d 0 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
    extensions: [jpg, jpeg, png]

  - name: passthrough
    command: ""                    # an empty command uploads the file unchanged
    extensions: [heic, mp4, mov]
```

## How a file is handled

1. The file's extension selects the tasks, in the order they appear in the file.
2. The first matching task runs. If its command fails, or does not leave exactly one file with content in the output folder, the next matching task is tried. When the command exits, whatever it left running in the background is stopped.
3. **If its output is not smaller than the original**, the next matching task is tried as well, and the first one that makes the file smaller is used. A task with an empty command (upload as it is) is always the last one tried.
4. **If the output is smaller than the original**, it is checked: it must be a photo or video that decodes completely, not a single flat colour (unless the original is), and not tiny next to the original (less than 1/16 of its width and height, which is a preview inside the file rather than the photo). If it passes, it is uploaded under the original name with the new extension. If no task made the file smaller, the original is uploaded unchanged.
5. **If every matching task fails**, or the output is empty or fails the check, nothing is uploaded and the original is kept in the [undone folder](undone.md).
6. **If no task matches the extension**, the file is refused (and kept in the undone folder unless [`IO_UNDONE_UNMATCHED`](configuration.md#undone-folder-and-audit-log) is `false`). List every type you want to accept, with an empty command for those you do not convert.

## Writing a command

| Placeholder | Meaning |
|---|---|
| `{{.src_folder}}` | temporary folder that holds the input file |
| `{{.dst_folder}}` | temporary output folder. The command must leave **exactly one file** here |
| `{{.name}}` | the input's file name without extension (a temporary name, not the original one) |
| `{{.extension}}` | the input's extension, without the dot |

The command also finds the same values, and a few more, in its **environment**. A script can be written with these alone:

| Variable | Meaning |
|---|---|
| `IO_SRC_FOLDER`, `IO_DST_FOLDER`, `IO_NAME`, `IO_EXTENSION` | the same as the placeholders above |
| `IO_ORIGINAL_NAME` | the file's own name: as it was uploaded, or as it is stored in Immich |
| `IO_SOURCE` | where the file comes from: `proxy`, `watcher`, `library` or `download` |
| `IO_TASK` | the name of the task that is running |
| `IO_MOTION_PHOTOS` | the task's [`motion_photos`](#motion-photos) setting |

```yaml
  - name: by-script
    command: ./convert.sh          # reads "$IO_SRC_FOLDER/$IO_NAME.$IO_EXTENSION", writes into "$IO_DST_FOLDER"
    extensions: [jpg]
```

`IO_ORIGINAL_NAME` is only available as a variable, never as a placeholder. A file name is whatever the uploader made it, and a placeholder becomes part of a shell command line. Read it quoted (`"$IO_ORIGINAL_NAME"`), and do not use it to build paths. The command also inherits the optimizer's own environment, except the Immich API key (`IO_IMMICH_API_KEY`, `IO_IMMICH_API_KEY_FILE`).

A long command reads better over several lines. Write it as a block (`|`), one argument per line, each line but the last ending in `\`:

```yaml
  - name: jpeg-xl
    command: |
      cjxl --lossless_jpeg=1 -d 0 \
        "{{.src_folder}}/{{.name}}.{{.extension}}" \
        "{{.dst_folder}}/{{.name}}.jxl"
    extensions: [jpg, jpeg, png]
```

The bundled profiles are written this way. Use `|`, not `>`: with `>` YAML joins the lines itself, and a `\` at the end of a line then ends up in the middle of the command.

- Commands run through `sh -c`, in the folder of the tasks file, so scripts next to it can be called as `./script.sh`.
- Quote the paths.
- An unknown placeholder stops the optimizer at startup with an error.
- The output's extension is taken from the file the command wrote.
- The last 4 KiB of a failed command's output is saved with the file in the undone folder.
- [`IO_TASKS_TIMEOUT`](configuration.md#tasks) limits how long one file may take, all its tasks together. When it is reached, the running command is killed together with everything it started, and no further task is tried.

## Bundled profiles

The image ships these under `/etc/immich-optimizer/bundled-configs/<profile>/`. The `lossless` profile is the default. To use another one, point `IO_TASKS_FILE` at it, or copy it and mount your own folder at `/etc/immich-optimizer/config`.

| Profile | Photos | Videos |
|---|---|---|
| [`lossless`](../config/lossless/tasks.yaml) (default) | JPEG and PNG to JPEG XL without any loss; the rest unchanged | unchanged |
| [`profile1`](../config/profile1/tasks.yaml) | lossy JPEG XL (quality 75), HEIC included | HandBrake |

Every bundled profile accepts every file type Immich accepts.

**Camera RAW is uploaded as it is** by both profiles, DNG included. The tools in the image do not develop RAW files: given one, they read the small preview stored inside it. Up to version 0.4.0-dev.1, `profile1` gave `.dng` files to such a command, and the result was a thumbnail-sized JPEG XL in place of the photo. If you used `profile1` with DNG files, look for `.jxl` assets of a few kilobytes whose original name ended in `.dng`: with library mode the originals are in Immich's trash until it is emptied; with proxy and watcher mode only the phone or the source folder still has them.

**HDR photos (Ultra HDR).** Recent Android phones (Pixel, Samsung) save HDR photos as a JPEG with a second, small image inside, the *gain map*, that HDR screens use to show the bright parts brighter. JPEG XL has no place for it:

- `lossless` keeps it inside the JPEG XL: a [download](#downloads) through the proxy gives back the original HDR JPEG, byte for byte. In Immich the JPEG XL itself shows as a normal (SDR) photo.
- `profile1` drops it: the photo is kept, the HDR effect is lost for good. To keep HDR photos as they are with it, see [HDR photos](#hdr-photos).

Photos that are HDR in their own format (10-bit AVIF, JPEG XL) are uploaded as they are by both profiles. Checked with the samples Immich tests with: a Pixel 8a Ultra HDR JPEG, HDR AVIF and JPEG XL, and an iPhone HEIC in Display P3, whose colours come through unchanged. Newer iPhones store HDR in HEIC their own way, with a gain map too; `profile1` converts only the photo itself, so expect the same loss there (not checked: Immich has no sample of one).

**`profile1` and video from current phones:** its HandBrake preset encodes to AV1 at a very high quality. Measured on clips straight from an iPhone and a Pixel, the result was two to three times larger than the phone's own file, so the original was kept and nothing was saved. It is useful for older or high-bitrate video; expect no gain on recent phone video until the preset is changed.

## Motion photos

A motion photo is a photo with a short video embedded in the same file (Android/Pixel JPEGs, Samsung and other Android HEICs). The `motion_photos` key of your `tasks.yaml` controls **what each task command receives** for such a file:

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

- The bundled `lossless` and `profile1` profiles set `motion_photos: drop_video`: **keep the photo, drop the video**. A tasks file without the key keeps motion photos as they are.
- The setting only affects motion photos. Every other file reaches the task unchanged.
- When several tasks match a file, each one gets its input prepared by its own setting.
- With `drop_video` the motion flag is also switched off in the photo's metadata, so Immich does not look for a video that is gone.
- With `drop_photo` the input is `{{.name}}.mp4`, so `{{.extension}}` is `mp4` in the command.
- The log says what happened: `Optimized IMG_1234.jpg (proxy, motion photo: drop_video): 5.2 MB -> 1.9 MB`.
- If all tasks fail, the **untouched original, video included**, is saved to the undone folder.

Things to know:

- **`keep` with a converting task rarely keeps the video.** Lossy encoders drop it without notice, and `cjxl --lossless_jpeg=1` hides it inside the output where Immich cannot play it. To really keep a motion photo intact, use `keep` on a passthrough task (empty command).
- **Split by the optimizer itself**, before the task runs:
  - **JPEG** (Google `MotionPhoto` and the older `MicroVideo` format). A file is only split when an MP4 really starts where its metadata says; otherwise it is passed as it is.
  - **HEIC, HEIF and AVIF**, whose video is in an `mpvd` box of the file. The video is taken out as it is; removing it from the photo (`drop_video`) is done with ExifTool, which is in the Docker image. Without ExifTool that task fails and the next one is tried, so the file is never passed on with its video.
- The task command also gets the setting in the `IO_MOTION_PHOTOS` environment variable, for a format the optimizer does not split.
- **Samsung:** see [Samsung motion photos](#samsung-motion-photos).
- Apple Live Photos are different: they are two separate files and both are processed normally.

### Samsung motion photos

Samsung phones write motion photos with the standard metadata above, plus an index of their own (a trailer at the end of a JPEG, a `sefd` box in a HEIC) that lists the video too. Both are handled: the video is cut out without that index, and the photo keeps nothing that points at a video. Checked against the Samsung samples Immich itself tests with (One UI 5 and 6, JPEG and HEIC).

**Samsung files with only Samsung's own index**, without the standard motion-photo metadata, are not detected. None has been seen yet; older phones may write them. Such a file is handled as an ordinary photo: `motion_photos` does not apply, the task gets the whole file, and its video does not survive a converting task (in library mode it survives when Immich itself shows the photo as a motion photo: the copy is paired with that video).

To check a file, ask ExifTool. One like this has an `EmbeddedVideoType` of `MotionPhoto_Data` and no `MotionPhoto` tag:

```sh
exiftool -EmbeddedVideoType -XMP-GCamera:MotionPhoto IMG_1234.jpg
```

If you have one, a sample is welcome in an issue. Until then, in library mode, tag such photos `immich-optimizer/keep`.

## HDR photos

An HDR photo here is a JPEG with a gain map (Ultra HDR, or ISO 21496-1): Pixel and recent Samsung and other Android phones take them. The `gain_maps` key of your `tasks.yaml` says **what each task does with one**. Like `motion_photos`, set it at the top as the default and on a task to override it:

| Value | The task |
|---|---|
| `keep` | gets the file as it is, gain map included (built-in default). A lossy task drops the gain map: the photo comes out ordinary (SDR) |
| `drop` | gets only the ordinary photo, without the gain map and without the metadata that announces one |
| `skip` | is not used for HDR photos: the next matching task gets them |

`skip` lets a lossy task convert ordinary photos while HDR photos go to another task, for example one that keeps them whole:

```yaml
motion_photos: drop_video

tasks:
  - name: jpeg-xl                # ordinary photos: lossy JPEG XL
    gain_maps: skip
    command: cjxl --lossless_jpeg=0 -q 75 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
    extensions: [jpg, jpeg]

  - name: jpeg-xl-hdr            # HDR photos: lossless, the gain map kept inside
    command: cjxl --lossless_jpeg=1 -d 0 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
    extensions: [jpg, jpeg]
```

- **Motion photos come first:** the task gets the photo prepared by `motion_photos` (for example without its video), and `gain_maps` applies to that photo. A motion photo that keeps its video (`keep`) also keeps its gain map with `drop`, since removing what follows the photo would take the video too.
- `drop` is done with ExifTool, which is in the Docker image. Without it, that task fails and the next one is tried.
- The marker records `gain_maps: drop` on a photo whose gain map was dropped, and the log says so: `Optimized PXL_1234.jpg (proxy, motion photo: drop_video, HDR gain map dropped): ...`.
- Only JPEG is recognised. HDR in HEIC (newer iPhones) and AVIF is not, and is handled as an ordinary photo; files that are HDR in their own format (10-bit AVIF, JPEG XL) are simply ordinary files here.
- If every task for a file is set to `skip`, an HDR photo of that type fails (and goes to the undone folder): add a task that takes it.

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
- **Only the download of a single original is converted** ("Download" in the web and mobile apps, `GET /api/assets/{id}/original`). Archives of several assets, and a photo edited in Immich itself (rotated or cropped there), are sent as Immich makes them.
- **If the command fails, the stored file is sent**, and the log says so.
- **To switch it off** without changing the tasks file, set `IO_PROXY_CONVERT_DOWNLOADS=false`: downloads then come as Immich stores them.
- The file is downloaded from Immich in full and converted, also when only a part of it is asked for. The result is kept for a minute after its last use, so a player or a download manager that asks for one file in many pieces has it converted once. It takes one more request to Immich per download, to learn the file's type.
- **The bundled profiles that store JPEG XL have this list**, so with them originals are downloaded as JPEG. This includes a JXL you uploaded as one. Remove the list from your tasks file to download files as they are stored; a tasks file without it converts nothing.
- Only proxy mode reads it.
