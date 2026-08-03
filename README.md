# Immich Optimizer

[![Release](https://img.shields.io/github/v/release/miguelangel-nubla/immich-optimizer)](https://github.com/miguelangel-nubla/immich-optimizer/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://ghcr.io/miguelangel-nubla/immich-optimizer)
[![Go Version](https://img.shields.io/github/go-mod/go-version/miguelangel-nubla/immich-optimizer)](https://golang.org/)
[![License](https://img.shields.io/github/license/miguelangel-nubla/immich-optimizer)](LICENSE)

A file optimization service that automatically processes and uploads media files to [Immich](https://immich.app/). This tool watches for new files in a directory, applies configurable optimization tasks, and uploads the optimized results to your Immich instance.

## ✨ Features

- **📁 File Watching**: Automatically monitors directories for new media files (`IUO_MODE=watcher`)
- **🌐 HTTP Reverse Proxy Mode**: Intercepts upload requests on-the-fly to optimize media uploads (`IUO_MODE=proxy`)
- **⚡ Concurrent Modes**: Combine modes by comma-separating them (e.g., `IUO_MODE=watcher,proxy`)
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
> `immich-optimizer` now incorporates all functionality from `immich-upload-optimizer`. Set `IUO_MODE=proxy` to run as an HTTP reverse proxy in front of Immich. See the [Migration Guide](#-migrating-from-immich-upload-optimizer) below.

## 🔄 Migrating from `immich-upload-optimizer`

If you are migrating from `immich-upload-optimizer` to `immich-optimizer`, update your configuration as follows:

1. **Set Proxy Mode**:
   Set `IUO_MODE=proxy` (or CLI flag `-mode proxy`).

2. **Update Custom `tasks.yaml` Templates**:
   In `immich-optimizer`, input files and output files are cleanly isolated into separate directories:
   - Replace `{{.folder}}` with `{{.src_folder}}` for the input file path.
   - Replace `{{.folder}}` with `{{.dst_folder}}` for the output file path.
   
   *Example:*
   ```yaml
   # ❌ Old syntax (immich-upload-optimizer):
   command: cjxl --lossless_jpeg=1 {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}.jxl

   # ✅ New syntax (immich-optimizer):
   command: cjxl --lossless_jpeg=1 {{.src_folder}}/{{.name}}.{{.extension}} {{.dst_folder}}/{{.name}}.jxl
   ```

3. **Environment Variable Changes**:
   - `IUO_UPSTREAM` / `-upstream` $\rightarrow$ `IUO_IMMICH_URL` / `-immich_url`
   - `IUO_LISTEN` / `-listen` $\rightarrow$ `IUO_BIND_ADDR` / `-bind_addr`
   - `IUO_FILTER_PATH` (`-filter_path`) and `IUO_FILTER_FORM_KEY` (`-filter_form_key`) remain fully supported.


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
  -e IUO_IMMICH_URL=http://your-immich-instance:2283 \
  -e IUO_IMMICH_API_KEY=your-api-key \
  ghcr.io/miguelangel-nubla/immich-optimizer:latest
```

### Docker Compose

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    container_name: immich-optimizer
    environment:
      - IUO_IMMICH_URL=http://immich-server:2283
      - IUO_IMMICH_API_KEY=your-api-key
      - IUO_WATCH_DIR=/watch
      - IUO_TASKS_FILE=/etc/immich-optimizer/config/tasks.yaml
    volumes:
      - /path/to/watch:/watch
      - /path/to/undone:/undone
      # Optional: Custom configuration
      - ./custom-config:/etc/immich-optimizer/config
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

## ⚙️ Configuration

### Environment Variables

| Variable | Mode | Description | Default |
|----------|------|-------------|---------|
| `IUO_MODE` | All | Operating mode: `watcher`, `proxy` (comma-separated for multiple) | `watcher` |
| `IUO_IMMICH_URL` | All | Immich server URL (required) | - |
| `IUO_TASKS_FILE` | All | Path to tasks configuration file | `tasks.yaml` |
| `IUO_LOG_LEVEL` | All | Log level filtering (`debug`, `info`, `warn`, `error`) | `info` |
| `IUO_PROXY_BIND_ADDR` | `proxy` | Address for reverse proxy server to listen on | `:8080` |
| `IUO_PROXY_FILTER_PATH` | `proxy` | Path pattern to intercept for proxy uploads | `/api/assets` |
| `IUO_PROXY_FILTER_FORM_KEY` | `proxy` | Form key for upload files | `assetData` |
| `IUO_WATCHER_WATCH_DIR` | `watcher` | Directory to watch for files | `/watch` |
| `IUO_WATCHER_UNDONE_DIR` | `watcher` | Directory for files that failed processing/upload | `/undone` |
| `IUO_IMMICH_API_KEY` | `watcher` | Immich API key (required in `watcher` mode) | - |

### Command Line Options

```bash
immich-optimizer [options]

General Options:
  -mode string           Operating mode: watcher or proxy (comma-separated for multiple) (default "watcher")
  -immich_url string     Immich server URL (required)
  -tasks_file string     Tasks configuration file (default "tasks.yaml")
  -log_level string      Log level: debug, info, warn, error (default "info")
  -version               Show version information

Proxy Mode Options:
  -bind_addr string      Address for reverse proxy server to listen on (default ":8080")
  -filter_path string    Path pattern to intercept for proxy uploads (default "/api/assets")
  -filter_form_key string Form key for upload files (default "assetData")

Watcher Mode Options:
  -immich_api_key string Immich API key (required in watcher mode)
  -watch_dir string      Directory to watch (default "/watch")
  -undone_dir string     Directory for failed files (default "/undone")
```

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
    command: ""  # Empty command passes file through unchanged
    extensions:
      - webp
      - avif
```

### Template Variables

Available in task commands:

- `{{.src_folder}}` - Source directory path
- `{{.dst_folder}}` - Destination directory path  
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

The optimizer prints timestamped logs to stdout. You can filter log levels (`debug`, `info`, `warn`, `error`) using `IUO_LOG_LEVEL` or the `-log_level` flag:

```bash
# Set log level for binary
export IUO_LOG_LEVEL=debug
immich-optimizer -immich_url http://... -immich_api_key ...

# Set log level for Docker container
docker run -e IUO_LOG_LEVEL=debug ghcr.io/miguelangel-nubla/immich-optimizer:latest
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
