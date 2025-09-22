# Immich Optimizer

[![Release](https://img.shields.io/github/v/release/miguelangel-nubla/immich-optimizer)](https://github.com/miguelangel-nubla/immich-optimizer/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://ghcr.io/miguelangel-nubla/immich-optimizer)
[![Go Version](https://img.shields.io/github/go-mod/go-version/miguelangel-nubla/immich-optimizer)](https://golang.org/)
[![License](https://img.shields.io/github/license/miguelangel-nubla/immich-optimizer)](LICENSE)

A file optimization service that automatically processes and uploads media files to [Immich](https://immich.app/). This tool watches for new files in a directory, applies configurable optimization tasks, and uploads the optimized results to your Immich instance.

## ✨ Features

- **📁 File Watching**: Automatically monitors directories for new media files
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

## 📦 Installation

### Docker (Recommended)

```bash
# Pull the latest image
docker pull ghcr.io/miguelangel-nubla/immich-optimizer:latest

# Run with lossless optimization
docker run -d \
  --name immich-optimizer \
  -v /path/to/watch:/watch \
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
      # Optional: Custom configuration
      - ./custom-config:/etc/immich-optimizer/config
    restart: unless-stopped
```

## ⚙️ Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `IUO_IMMICH_URL` | Immich server URL (required) | - |
| `IUO_IMMICH_API_KEY` | Immich API key (required) | - |
| `IUO_WATCH_DIR` | Directory to watch for files | `/watch` |
| `IUO_TASKS_FILE` | Path to tasks configuration | `tasks.yaml` |

### Command Line Options

```bash
immich-optimizer [options]

Options:
  -immich_url string     Immich server URL
  -immich_api_key string Immich API key  
  -watch_dir string      Directory to watch (default "/watch")
  -tasks_file string     Tasks configuration file (default "tasks.yaml")
  -version               Show version information
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

### Debug Mode

Enable verbose logging by setting log level:

```bash
# For binary
export LOG_LEVEL=debug
immich-optimizer

# For Docker
docker run -e LOG_LEVEL=debug ...
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