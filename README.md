# Immich Optimizer

[![Release](https://img.shields.io/github/v/release/miguelangel-nubla/immich-optimizer)](https://github.com/miguelangel-nubla/immich-optimizer/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue)](https://ghcr.io/miguelangel-nubla/immich-optimizer)
[![License](https://img.shields.io/github/license/miguelangel-nubla/immich-optimizer)](LICENSE)

Shrinks the photos and videos in your [Immich](https://immich.app/) library by converting them with tools you choose, for example to lossless JPEG XL. You decide what happens to each file type in one [tasks file](docs/tasks.md).

> Coming from `immich-upload-optimizer`? This project replaces it. See the [migration guide](docs/migration.md).

## Three ways to use it

| Mode | What it does | Use it for |
|---|---|---|
| [Proxy](docs/proxy.md) | sits in front of Immich and converts uploads on the way in | the web interface, the mobile app, `immich-go`, the CLI |
| [Watcher](docs/watcher.md) | watches a folder, converts and uploads what appears in it | bulk imports, phone backup through a sync tool |
| [Library](docs/library.md) | converts the assets already in Immich | shrinking an existing library |

Modes can be combined: `IO_MODE=watcher,proxy`.

In every mode:

- **A failed conversion never uploads a broken or unconverted file.** In proxy and watcher mode the original is kept in the [undone folder](docs/undone.md); in library mode it simply stays in Immich.
- Every converted asset is tagged in Immich with a [marker](docs/marker.md) that records its original's checksum.
- Every operation on an asset is written to a persistent [audit log](docs/audit-log.md).

**Using the official Immich mobile app?** It needs one setting to work well with converted photos, which the quick start includes. Details: [The mobile app](docs/proxy.md#the-mobile-app).

## Quick start

**New here? Follow the [quick start guide](docs/quickstart.md):** ten minutes, no background needed, and every photo you upload from then on is stored smaller without losing quality.

In short, it is one more service in your Immich `docker-compose.yml`, in front of Immich:

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_MODE=proxy
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_PROXY_EXPERIMENTAL_APP_SYNC=true   # for the phone app
    ports:
      - "2283:2283"
    volumes:
      - ./undone:/undone
    restart: unless-stopped
```

## Documentation

| | |
|---|---|
| [Quick start](docs/quickstart.md) | the recommended setup, step by step |
| [Proxy mode](docs/proxy.md) · [Watcher mode](docs/watcher.md) · [Library mode](docs/library.md) | setting up and understanding each mode |
| [Tasks](docs/tasks.md) | the tasks file, bundled profiles, motion photos, converting downloads back |
| [Configuration](docs/configuration.md) | every setting |
| [Undone folder](docs/undone.md) · [Audit log](docs/audit-log.md) · [Marker](docs/marker.md) | what the optimizer keeps and records |
| [Docker images](docs/docker.md) | tags, folders, graphics cards and custom images, health check, dev builds |
| [Migrating from immich-upload-optimizer](docs/migration.md) | |
| [Development](docs/development.md) | code layout, tests, contributing |

## Troubleshooting

- **Nothing happens, or an error at startup:** set `IO_LOG_LEVEL=debug` and read the container's log. It names the setting or the task that is wrong.
- **A file was not uploaded:** look for it in the [undone folder](docs/undone.md); its `metadata.json` holds the command that failed and its output.
- **Cannot reach Immich:** from the optimizer's container, `curl http://immich-server:2283/api/server/ping` must answer.
- **Permission denied:** the container runs as user id `1001`. See [Folders and ports](docs/docker.md#folders-and-ports).

Found a bug? [Open an issue](https://github.com/miguelangel-nubla/immich-optimizer/issues).

## License and thanks

MIT, see [LICENSE](LICENSE). Built on [Immich](https://immich.app/), [JPEG XL](https://jpegxl.info/), [Caesium](https://saerasoft.com/caesium/), [HandBrake](https://handbrake.fr/), [libvips](https://www.libvips.org/), [FFmpeg](https://ffmpeg.org/) and [ExifTool](https://exiftool.org/).
