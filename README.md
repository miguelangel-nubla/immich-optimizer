# Immich Optimizer

[![Release](https://img.shields.io/github/v/release/miguelangel-nubla/immich-optimizer)](https://github.com/miguelangel-nubla/immich-optimizer/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://ghcr.io/miguelangel-nubla/immich-optimizer)
[![Go Version](https://img.shields.io/github/go-mod/go-version/miguelangel-nubla/immich-optimizer)](https://golang.org/)
[![License](https://img.shields.io/github/license/miguelangel-nubla/immich-optimizer)](LICENSE)

A file optimization service that automatically processes and uploads media files to [Immich](https://immich.app/). This tool watches for new files in a directory, applies configurable optimization tasks, and uploads the optimized results to your Immich instance.

## ✨ Features

- **📁 File Watching**: Automatically monitors directories for new media files (`IO_MODE=watcher`)
- **🌐 HTTP Reverse Proxy Mode**: Intercepts upload requests on-the-fly to optimize media uploads (`IO_MODE=proxy`)
- **⚡ Concurrent Modes**: Combine modes by comma-separating them (e.g., `IO_MODE=watcher,proxy`)
- **🛟 Never uploads the original on failure**: Files that fail conversion are kept in the [undone folder](#-failed-files-undone-folder) with a `metadata.json` explaining why, whatever the source
- **🔄 Configurable Processing**: Support for multiple optimization profiles
- **📸 Image Optimization**:
  - Lossless JPEG-XL conversion
  - Caesium compression
  - Format-specific optimization
- **🎥 Video Optimization**: HandBrake integration for video compression
- **🚀 Multi-Architecture**: Native support for AMD64 and ARM64
- **🔒 Secure**: Runs as non-root user with proper file permissions
- **⚡ Performance**: Concurrent processing with configurable limits
- **📊 Monitoring**: Built-in health checks and structured logging
- **🐳 Docker Ready**: Production-ready container images

> 💡 **Note on `immich-upload-optimizer` Deprecation**:
> `immich-optimizer` now incorporates all functionality from `immich-upload-optimizer`. Set `IO_MODE=proxy` to run as an HTTP reverse proxy in front of Immich. See the [Migration Guide](#-migrating-from-immich-upload-optimizer) below.

## 🔄 Migrating from `immich-upload-optimizer`

If you are migrating from `immich-upload-optimizer` to `immich-optimizer`, update your configuration as follows:

1. **Set Proxy Mode**:
   Set `IO_MODE=proxy` (or CLI flag `-mode proxy`). The proxy listens on `:2283` by default, the same port as Immich and `immich-upload-optimizer`, so an existing `2283:2283` port mapping keeps working. It runs in its own container, so there is no clash with Immich; set `IO_PROXY_BIND_ADDR` only if you share a network namespace with Immich.

2. **Update Custom `tasks.yaml` Templates**:
   In `immich-optimizer`, input files and output files are cleanly isolated into separate directories:
   - Replace `{{.folder}}` with `{{.src_folder}}` for the input file path.
   - Replace `{{.folder}}` with `{{.dst_folder}}` for the output file path.
   - Files with no matching task extension are blocked. Add an empty-command task for formats you want to pass through unchanged.
   
   *Example:*
   ```yaml
   # ❌ Old syntax (immich-upload-optimizer):
   command: cjxl --lossless_jpeg=1 {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}.jxl

   # ✅ New syntax (immich-optimizer):
   command: cjxl --lossless_jpeg=1 {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.jxl
   ```

3. **Environment Variable Changes**: see [Migrating: environment variable rename](#-migrating-environment-variable-rename). In short, `IUO_UPSTREAM` / `-upstream` becomes `IO_IMMICH_URL` / `-immich_url`, and `IUO_LISTEN` / `-listen` becomes `IO_PROXY_BIND_ADDR` / `-bind_addr`. The old names keep working for now, with a startup warning.

4. **Failed uploads are kept**: when a conversion fails (or no task matches), the client still gets an error and nothing is uploaded, but the original is now also saved to the undone folder. Mount `/undone` in proxy mode too.

## 🔁 Migrating: environment variable rename

The environment prefix is now `IO_` (Immich Optimizer). The old `IUO_*` names are still accepted as a deprecated fallback for one transition period: the optimizer logs a single warning at startup listing which old names were used and their replacements. If both the `IO_*` and the `IUO_*` variable are set, `IO_*` wins (the warning says the old one was ignored).

| Old (deprecated) | New |
|---|---|
| `IUO_<NAME>` (every variable that existed before; new settings have no `IUO_` name) | `IO_<NAME>` |
| `IUO_UPSTREAM`, `-upstream` | `IO_IMMICH_URL`, `-immich_url` |
| `IUO_LISTEN`, `IUO_BIND_ADDR`, `-listen` | `IO_PROXY_BIND_ADDR`, `-bind_addr` |
| `IUO_FILTER_PATH`, `IUO_FILTER_FORM_KEY` | `IO_PROXY_FILTER_PATH`, `IO_PROXY_FILTER_FORM_KEY` |
| `IUO_WATCH_DIR`, `IUO_INOTIFY_BUFFER_SIZE` | `IO_WATCHER_WATCH_DIR`, `IO_WATCHER_INOTIFY_BUFFER_SIZE` |
| `IUO_WATCHER_UNDONE_DIR`, `IUO_UNDONE_DIR` | `IO_UNDONE_DIR` (now used by every mode) |

The Docker image no longer sets any configuration environment variables; its built-in defaults already point at `/watch`, `/undone` and `/etc/immich-optimizer/config/tasks.yaml`.


## 📦 Installation

See [ARCHITECTURE.md](ARCHITECTURE.md) to understand the whole picture.

### Local Binary

```bash
# Build binary from source
go build -o immich-optimizer ./cmd/optimizer

# Run binary
./immich-optimizer -immich_url http://your-immich-instance:2283 -immich_api_key your-api-key
```

### Docker (Recommended)

```bash
# Pull the latest image
docker pull ghcr.io/miguelangel-nubla/immich-optimizer:latest

# Run with lossless optimization
docker run -d \
  --name immich-optimizer \
  -v /path/to/watch:/watch \
  -v /path/to/undone:/undone \
  -e IO_IMMICH_URL=http://your-immich-instance:2283 \
  -e IO_IMMICH_API_KEY=your-api-key \
  ghcr.io/miguelangel-nubla/immich-optimizer:latest
```

### Docker Compose

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    container_name: immich-optimizer
    environment:
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_IMMICH_API_KEY=your-api-key
    volumes:
      - /path/to/watch:/watch
      - /path/to/undone:/undone
      # Optional: Custom configuration
      - ./custom-config:/etc/immich-optimizer/config
    restart: unless-stopped
```

Proxy mode (point your Immich apps at this container instead of Immich):

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    container_name: immich-optimizer
    environment:
      - IO_MODE=proxy
      - IO_IMMICH_URL=http://immich-server:2283
    ports:
      - "2283:2283"
    volumes:
      - /path/to/undone:/undone
    # Let running conversions finish on restart (see "Long jobs & restarts").
    stop_grace_period: 2m
    restart: unless-stopped
```

### 🚀 Custom Image (GPU Acceleration, FFMPEG, etc.)

Hardware-accelerated video encoding (NVidia NVENC, Intel VAAPI, etc.) is **not included in the base image** because providing a one-size-fits-all solution is complex and leads to massive image fragmentation. Furthermore, there are some limitations with the upstream HandBrake base image not supporting `arm64` (see [jlesage/docker-handbrake#48](https://github.com/jlesage/docker-handbrake/issues/48)).

Instead of using the pre-built image, you can use your own Dockerfile, I provide a example [Dockerfile.custom](Dockerfile.custom) (should already have GPU acceleration working) as a starting point to bundle the latest **Immich Optimizer** binary directly **INTO your own specialized container environment**. This approach allows you to install **any additional packages or specific versions** (e.g. CUDA, specialized ffmpeg builds, specific driver versions, or custom tools) required for your specific hardware/workflow.

This is also a great alternative for users who want to **rely solely on `ffmpeg`** for video optimization without the overhead or specific requirements of the HandBrake base image. Or just want to run immich-optimizer binary directly without any container at all.

The following script downloads the latest **Immich Optimizer** binary from the GitHub releases page and installs it:

```Dockerfile
ARG IMMICH_OPTIMIZER_REPO=miguelangel-nubla/immich-optimizer
RUN set -eux; \
    LATEST_TAG=$(curl -s https://api.github.com/repos/$IMMICH_OPTIMIZER_REPO/releases/latest | jq -r '.tag_name'); \
    case "$TARGETPLATFORM" in \
    "linux/amd64") ARCH=x86_64 ;; \
    "linux/arm64") ARCH=arm64 ;; \
    *) echo "Platform $TARGETPLATFORM not supported"; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/immich-optimizer.tar.gz \
    "https://github.com/$IMMICH_OPTIMIZER_REPO/releases/download/${LATEST_TAG}/immich-optimizer_Linux_${ARCH}.tar.gz"; \
    tar xzf /tmp/immich-optimizer.tar.gz -C /usr/local/bin immich-optimizer; \
    rm /tmp/immich-optimizer.tar.gz; \
    chmod +x /usr/local/bin/immich-optimizer
```

### 🧪 Dev builds (unstable)

Every push to the `dev` branch publishes test images for amd64 and arm64:

- `ghcr.io/miguelangel-nubla/immich-optimizer:dev`: the latest `dev` commit (moves on every push)
- `ghcr.io/miguelangel-nubla/immich-optimizer:dev-<shortsha>`: one specific commit, e.g. `:dev-1a2b3c4`

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:dev
    pull_policy: always
    # ... same settings as above
```

`immich-optimizer -version` reports `dev-<shortsha>`. The binaries of each build are attached to its [Dev workflow run](https://github.com/miguelangel-nubla/immich-optimizer/actions/workflows/dev.yaml) as an artifact (GitHub sign-in required). Dev builds are **unstable**: they may contain unfinished features, break your configuration or change the undone folder format without notice. Do not run them on a library you have no backup of, and pin `:dev-<shortsha>` rather than `:dev` if you need to stay on a build that works. Dev builds never create a GitHub Release and never change `:latest` or the version tags.

## ⚙️ Configuration

### Environment Variables

| Variable | Mode | Description | Default |
|----------|------|-------------|---------|
| `IO_MODE` | All | Operating mode: `watcher`, `proxy` (comma-separated for multiple) | `watcher` |
| `IO_IMMICH_URL` | All | Immich server URL (required) | - |
| `IO_TASKS_FILE` | All | Path to tasks configuration file | `./tasks.yaml` if it exists (and is not inside the watch directory), else `/etc/immich-optimizer/config/tasks.yaml` |
| `IO_LOG_LEVEL` | All | Log level filtering (`debug`, `info`, `warn`, `error`) | `info` |
| `IO_MAX_CONCURRENT_REQUESTS` | All | Maximum number of concurrent conversions. One limit shared by all modes (`watcher,proxy` does not double it) | `10` |
| `IO_HTTP_TIMEOUT_SECONDS` | All | Timeout for connecting to Immich, for an upload that makes no progress at all (Immich stopped reading it), and for Immich's response headers after an upload body has been sent. It does **not** limit how long an upload or a conversion takes | `120` |
| `IO_TASK_TIMEOUT_SECONDS` | All | Maximum processing time per file; on expiry the task's whole process tree is killed and the file goes to undone. `0` = no limit | `0` |
| `IO_SHUTDOWN_TIMEOUT_SECONDS` | All | On stop, how long running jobs may finish before they are cancelled (see [Long jobs & restarts](#-long-jobs--restarts)) | `8` |
| `IO_UNDONE_DIR` | All | Directory where inputs that failed conversion are saved as `<sha1>/original.file` + `metadata.json` | `/undone` |
| `IO_UNDONE_UNMATCHED` | All | Also save files that have no matching task to undone (they are always refused) | `true` |
| `IO_PROXY_BIND_ADDR` | `proxy` | Address for reverse proxy server to listen on | `:2283` |
| `IO_PROXY_FILTER_PATH` | `proxy` | Path pattern to intercept for proxy uploads | `/api/assets` |
| `IO_PROXY_FILTER_FORM_KEY` | `proxy` | Form key for upload files | `assetData` |
| `IO_WATCHER_WATCH_DIR` | `watcher` | Directory to watch for files (the undone dir must not be inside it) | `/watch` |
| `IO_WATCHER_INOTIFY_BUFFER_SIZE` | `watcher` | Inotify event read buffer size, in bytes | `8192` |
| `IO_IMMICH_API_KEY` | `watcher` | Immich API key (required in `watcher` mode) | - |
| `IO_IMMICH_API_KEY_FILE` | `watcher` | Path to a file containing the Immich API key (e.g. a Docker secret); takes precedence over `IO_IMMICH_API_KEY` | - |

The old `IUO_*` names still work for now; see [Migrating: environment variable rename](#-migrating-environment-variable-rename).

#### Reading secrets from files

Secret settings can be read from a file instead of passing the value directly, Docker secrets style. Set `IO_IMMICH_API_KEY_FILE` (or `-immich_api_key_file`) to the path of a file holding the API key. One trailing newline is stripped. When a key file is set it takes precedence over `IO_IMMICH_API_KEY` / `-immich_api_key`. An unreadable or empty file is a startup error.

```yaml
services:
  immich-optimizer:
    environment:
      - IO_IMMICH_API_KEY_FILE=/run/secrets/immich_api_key
    secrets:
      - immich_api_key
secrets:
  immich_api_key:
    file: ./immich_api_key.txt
```

### Command Line Options

```bash
immich-optimizer [options]

General Options:
  -mode string           Operating mode: watcher or proxy (comma-separated for multiple) (default "watcher")
  -immich_url string     Immich server URL (required)
  -tasks_file string     Tasks configuration file (default ./tasks.yaml, else /etc/immich-optimizer/config/tasks.yaml)
  -log_level string      Log level: debug, info, warn, error (default "info")
  -undone_dir string     Directory where inputs that failed conversion are saved (default "/undone")
  -undone_unmatched      Also save files with no matching task to undone (default true)
  -max_concurrent_requests int
                         Maximum number of concurrent conversions, shared by all modes (default 10)
  -http_timeout_seconds int
                         Connect, upload-stall and response-header timeout for Immich, in seconds (default 120)
  -task_timeout_seconds int
                         Maximum processing time per file in seconds, 0 = no limit (default 0)
  -shutdown_timeout_seconds int
                         How long running jobs may finish on shutdown, in seconds (default 8)
  -version               Show version information

Proxy Mode Options:
  -bind_addr string      Address for reverse proxy server to listen on (default ":2283")
  -filter_path string    Path pattern to intercept for proxy uploads (default "/api/assets")
  -filter_form_key string Form key for upload files (default "assetData")

Watcher Mode Options:
  -immich_api_key string Immich API key (required in watcher mode)
  -immich_api_key_file string
                         Path to a file containing the Immich API key (takes precedence over -immich_api_key)
  -watch_dir string      Directory to watch (default "/watch")
  -inotify_buffer_size int
                         Inotify event read buffer size, in bytes (default 8192)
```

The deprecated `-upstream` and `-listen` flags are still accepted (hidden from `-help`) and map to `-immich_url` and `-bind_addr`.

## 🛟 Failed files (undone folder)

The undone folder belongs to the conversion engine, so it works the same for every source (watcher, proxy, and future ones). **The original is never uploaded when conversion fails.** Instead it is saved to `IO_UNDONE_DIR` together with a `metadata.json` explaining why.

- **When a file is saved:** every task for its extension failed, a task produced an empty output, the task timeout expired, or no task matches its extension (unless `IO_UNDONE_UNMATCHED=false`). In watcher mode, a file whose upload to Immich fails (for example quota exceeded) is also saved.
- **Layout:** flat and content-addressed, one folder per distinct file content, named after its SHA-1 (the same checksum Immich stores for every asset):

  ```
  undone/
    3f8a…c1/                 SHA-1 (40 hex characters) of the file content
      original.file          the original, byte for byte, with its original modification time
      metadata.json          names, sources, folders and failures
  ```

  `original.file` is literally named that (no extension), so unusual or very long client filenames can never cause problems. There are no per-source subfolders and the watch directory's tree is not mirrored: the source and the original filename and folder are recorded in `metadata.json`. A folder only appears once it is complete (it is assembled under a temporary name and renamed into place); temporary `.io-undone-*` entries left by a crash are removed at startup (leftover copies once they are over an hour old). The undone directory may be shared by several optimizer processes, for example a watcher and a proxy container mounting the same folder: they coordinate through an advisory lock file, `.io-undone.lock`, at its root (leave it in place).
- **metadata.json** (pretty-printed JSON, `"version": 3`, written atomically) contains `sha1` (hex, the folder name), `sha1_base64` (the same checksum the way the Immich API shows it), `size`, `original_mtime`, `first_failed_at`, `last_failed_at`, `occurrences`, `original_filenames` (the distinct names this content arrived under, up to 100) and `failures`: the 20 most recent failures, each with `source` (`watcher` / `proxy`), `original_filename`, `original_dir` (the watcher's path relative to the watch directory, or the folder part of the proxy client's filename if it sent one), `failed_at`, `stage` (`no_task`, `convert` or `upload`), `error`, and every task attempted (command, exit code, duration, timeout flag, last 4 KiB of output). It contains no credentials or headers.
- **No duplicates:** if the identical content is already there (for example a phone retrying the same upload, or the same photo arriving from both the watcher and the proxy), it is not copied again (it does not even need temporary space); `occurrences` goes up and the failure is appended to its `metadata.json`. A *different* file with the same name always gets its own folder, so nothing is ever overwritten.
- **Finding a file in Immich:** Immich identifies every asset by the SHA-1 of its file, so the folder name is directly comparable with Immich's `checksum`. This only matches if these exact original bytes were uploaded to Immich some other way (for example from a client that bypassed the proxy), since an optimized upload has a different checksum. To check before retrying, ask Immich's API with the hex folder name or `sha1_base64`:

  ```sh
  curl -s -X POST "$IMMICH_URL/api/assets/bulk-upload-check" \
    -H "x-api-key: $IMMICH_API_KEY" -H 'Content-Type: application/json' \
    -d '{"assets":[{"id":"x","checksum":"<folder name>"}]}'
  # {"results":[{"id":"x","action":"reject","reason":"duplicate","assetId":"…"}]} means Immich already has it
  ```

  or `POST /api/search/metadata` with `{"checksum":"<sha1_base64>"}`.
- **Folders from earlier versions:** before metadata version 3, folders were named after the SHA-256 of the content (64 hex characters, `"sha256"` in `metadata.json`). They are left exactly as they are: never updated, cleaned up or deduplicated against. If the same content fails again it gets a new SHA-1 folder next to the old one. Retry or delete old folders by hand.
- **Watcher:** the source file is removed from the watch directory only once it has been uploaded or saved to undone. If saving fails (for example the disk is full), the file stays in the watch directory.
- **Proxy:** the client still gets an error (`415` for an unmatched extension, `502` for a conversion failure) and keeps its original, so it can retry. Upload failures to Immich (network errors, Immich `5xx`) are **not** saved in proxy mode: the client keeps the original and retries, so saving would only pile up copies.
- **Retrying a file:** fix the cause (for example the task), then copy `original.file` into the watch directory under its original name from `metadata.json` (`original_filenames`, or `original_filename` of a failure; the extension selects the task), for example `cp undone/3f8a…c1/original.file /watch/IMG_1234.HEIC`. Once it is uploaded you can delete the hash folder. In proxy mode you can instead just retry the upload from the client.
- **Permissions:** files are created by the container user (`appuser`, uid 1001) with the default umask. The directory must be writable by that user; this is checked at startup.

## ⏳ Long jobs & restarts

- **Jobs survive client disconnects (proxy):** once the upload has been received, converting the file and uploading it to Immich no longer depend on the client connection. If the phone disconnects or its request times out, the job continues and the asset still reaches Immich.
- **No double uploads:** if the same user uploads the identical file again while it is being processed, or within 30 minutes afterwards, the retry gets the same result instead of converting and uploading it a second time. Transient answers (`5xx`, `408`, `413`, `425`, `429`) and failures whose original could not be saved to undone are not replayed, so a retry reaches Immich again. Browser uploads are kept alive with periodic redirects, and a reload of the wait page shows the result for 30 minutes.
- **Timeouts:** only `IO_TASK_TIMEOUT_SECONDS` (off by default) or shutdown stops a running job. `IO_HTTP_TIMEOUT_SECONDS` only bounds connecting to Immich, an upload that makes no progress at all, and waiting for its response after the upload has been sent, so large but progressing uploads are not cut off.
- **Shutdown:** on `SIGTERM` the optimizer stops accepting new work and lets running jobs finish for `IO_SHUTDOWN_TIMEOUT_SECONDS`. After that, running tasks (including child processes such as HandBrake or ffmpeg) are killed. Proxy clients then get `503` with `Retry-After`, watcher files stay in the watch directory and are processed at the next start, and nothing is saved to undone. The default of 8s fits Docker's default 10s stop timeout. If you convert videos, raise both, for example `stop_grace_period: 15m` with `IO_SHUTDOWN_TIMEOUT_SECONDS=880`.

## 📸 Motion photos

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
- The log says what was passed: `motion photo IMG_1234.jpg (drop_video): passing only the photo to the task (photo 3.1 MB, video 2.1 MB)`.
- If all tasks fail, the **untouched original, video included**, is saved to the undone folder.

Things to know:

- **`keep` with a converting task rarely keeps the video.** Lossy encoders drop it without notice, and `cjxl --lossless_jpeg=1` hides it inside the output where Immich cannot play it. To really keep a motion photo intact, use `keep` on a passthrough task (empty command).
- **JPEG** motion photos (Google `MotionPhoto` and the older `MicroVideo` format) are split by the optimizer itself. A file is only split when an MP4 really starts where its metadata says; otherwise it is passed as it is.
- **HEIC** motion photos are not split by the optimizer. The task command gets the setting in the `IO_MOTION_PHOTOS` environment variable and applies it itself, as the bundled `profile1/heic2jxl.sh` does.
- Older Samsung JPEGs that store the video in a Samsung-specific trailer without the standard motion-photo metadata are not detected.
- Apple Live Photos are different: they are two separate files and both are processed normally.

## 📋 Optimization Profiles

The optimizer includes three pre-configured profiles:

### 🔒 Lossless Profile (Default)
```yaml
# Located at: config/lossless/tasks.yaml
# - Lossless JPEG-XL conversion for images
# - Caesium lossless compression
# - Passthrough for videos (no compression)
```

### ⚡ Lossy Profile
```yaml
# Located at: config/profile1/tasks.yaml  
# - Lossy JPEG-XL conversion (quality 75)
# - Caesium compression (quality 85)
# - HandBrake video compression
# - HEIC to JPEG-XL conversion
```

### 📤 Passthrough Profile
```yaml
# Located at: config/passthrough-all/tasks.yaml
# - No optimization, uploads files as-is
# - Allows only the extensions listed in the profile
# - Useful for testing or when optimization is not desired
```

## 🛠️ Custom Configuration

Create a custom `tasks.yaml` file:

```yaml
tasks:
  - name: jpeg-xl-lossless
    command: cjxl --lossless_jpeg=1 {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.jxl
    extensions:
      - jpeg
      - jpg
      - png
      
  - name: video-compress
    command: HandBrakeCLI -i {{.src_folder}}/{{.name}}.{{.extension}} -o {{.dst_folder}}/{{.name}}.mp4 --preset="Fast 1080p30"
    extensions:
      - avi
      - mkv
      - mov
      
  - name: passthrough
    command: ""  # Empty command passes matching files through unchanged
    extensions:
      - webp
      - avif
```

### Template Variables

Available in task commands:

- `{{.src_folder}}` - Source directory path
- `{{.dst_folder}}` - Destination directory path; non-empty commands must write exactly one output file here
- `{{.name}}` - Filename without extension
- `{{.extension}}` - File extension without dot

## 🔧 Troubleshooting

### Common Issues

**Connection Refused**
```bash
# Check Immich URL and network connectivity
curl -I http://your-immich-instance:2283/api/server-info
```

**Permission Denied**
```bash
# Ensure watch directory is accessible
ls -la /path/to/watch
# Fix permissions if needed
chmod 755 /path/to/watch
```

**Task Failures**
```bash
# Check if required tools are installed
docker exec immich-optimizer which cjxl
docker exec immich-optimizer which caesiumclt
```

### Logs & Monitoring

The optimizer prints timestamped logs to stdout. You can filter log levels (`debug`, `info`, `warn`, `error`) using `IO_LOG_LEVEL` or the `-log_level` flag:

```bash
# Set log level for binary
export IO_LOG_LEVEL=debug
immich-optimizer -immich_url http://... -immich_api_key ...

# Set log level for Docker container
docker run -e IO_LOG_LEVEL=debug ghcr.io/miguelangel-nubla/immich-optimizer:latest
```

### Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## 🙏 Acknowledgments

- [Immich](https://immich.app/) - The amazing self-hosted photo and video management solution
- [JPEG XL](https://jpegxl.info/) - Next-generation image compression
- [Caesium](https://saerasoft.com/caesium/) - Image compression tool
- [HandBrake](https://handbrake.fr/) - Video transcoder

## 📞 Support

- 🐛 [Report Issues](https://github.com/miguelangel-nubla/immich-optimizer/issues)
- 📖 [Documentation](https://github.com/miguelangel-nubla/immich-optimizer/wiki)
