# Docker images

## Tags

| Tag | What it is |
|---|---|
| `ghcr.io/miguelangel-nubla/immich-optimizer:latest` | the latest stable release |
| `:vX.Y.Z`, `:vX.Y`, `:vX` | a specific release, or the latest of a series |
| `:vX.Y.Z-beta.N` | a pre-release. Pre-releases never move `:latest` or the series tags |
| `:dev`, `:dev-<shortsha>` | [dev builds](#dev-builds) |

Images are built for amd64 and arm64.

## What is inside

- The optimizer, and the tools the [bundled profiles](tasks.md#bundled-profiles) call: `cjxl`/`djxl`, `caesiumclt`, `vips`, ImageMagick, HandBrake, `ffmpeg` and ExifTool.
- The bundled profiles in `/etc/immich-optimizer/bundled-configs`, with `lossless` copied to `/etc/immich-optimizer/config`.
- `ffmpeg` has the hardware encoders for NVIDIA and for VAAPI, and the image has the VAAPI driver for AMD. No bundled task uses a graphics card: see [Custom image](#custom-image).

## Folders and ports

| Path or port | Purpose |
|---|---|
| `/etc/immich-optimizer/config` | your [tasks file](tasks.md) and its scripts. Mount a folder here to replace the default |
| `/undone` | [undone folder](undone.md) and [audit log](audit-log.md). Mount it in every mode |
| `/watch` | the [watched folder](watcher.md) |
| `2283` | the [proxy](proxy.md) |

The container runs as user id `1001`, not root. Mounted folders must be writable by it, and mounted scripts readable and executable. For GPU devices such as `/dev/dri`, add the device's group with `group_add`.

## Stopping and restarting

On `SIGTERM` the optimizer stops taking new work and lets running jobs finish for [`IO_SHUTDOWN_TIMEOUT`](configuration.md#general). After that, running commands and everything they started are killed: proxy clients get `503` and retry, watcher files stay in the watch folder for the next start, and library mode undoes the asset it was working on.

The default of 8 seconds fits Docker's default 10 second stop timeout. If you convert videos, raise both, for example `stop_grace_period: 15m` with `IO_SHUTDOWN_TIMEOUT=14m40s`.

## Custom image

For anything the standard image does not have, such as video encoding on a graphics card, another tool, or your tasks built in, there is a template repository: [immich-optimizer-custom](https://github.com/miguelangel-nubla/immich-optimizer-custom).

- Press **Use this template** there (or fork it). You get your own repository with a `Dockerfile` and a tasks file.
- Every optional part is in them, commented out and explained: graphics cards (AMD, Intel, NVIDIA), extra packages, example tasks.
- GitHub builds the image on every change, and once a week, and publishes it to your repository as `ghcr.io/<you>/<your-repository>`. Nothing has to be built on your computer.

The standard image's `ffmpeg` already has the hardware encoders for NVIDIA and for VAAPI, and the VAAPI driver for AMD. For those cards the image itself needs no change: the container has to be given the device, and a task has to use it. The template's README shows both.

If you want no container at all, the optimizer is a single static file on the [releases page](https://github.com/miguelangel-nubla/immich-optimizer/releases).

## Dev builds

Every push to the `dev` branch publishes test images for amd64 and arm64:

- `ghcr.io/miguelangel-nubla/immich-optimizer:dev`: the latest `dev` commit (moves on every push)
- `ghcr.io/miguelangel-nubla/immich-optimizer:dev-<shortsha>`: one specific commit, e.g. `:dev-1a2b3c4`

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:dev
    pull_policy: always
    # ...
```

`immich-optimizer -version` reports `dev-<shortsha>`. The binaries of each build are attached to its [Dev workflow run](https://github.com/miguelangel-nubla/immich-optimizer/actions/workflows/dev.yaml) as an artifact (GitHub sign-in required). Dev builds are **unstable**: they may contain unfinished features, break your configuration or change the undone folder format without notice. Do not run them on a library you have no backup of, and pin `:dev-<shortsha>` rather than `:dev` if you need to stay on a build that works. Dev builds never create a GitHub Release and never change `:latest` or the version tags.

## Health check

The image has a Docker health check. It runs `immich-optimizer -healthcheck` inside the container, with the container's own settings:

- **Proxy mode:** it asks the proxy for `/_immich-optimizer/health`, which the proxy answers itself with `200` (and `503` while it is shutting down). It does not ask Immich: an Immich that is down is not a reason to restart the optimizer.
- **Watcher and library mode** have nothing to ask, so the check only confirms that the container runs.

You can point your own monitoring at the same path on the proxy's port.
