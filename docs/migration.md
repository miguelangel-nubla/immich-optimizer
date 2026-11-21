# Migrating from immich-upload-optimizer

`immich-optimizer` replaces `immich-upload-optimizer`. Its [proxy mode](proxy.md) does what the old project did. It is not a drop-in image swap: go through every point below.

**Before (immich-upload-optimizer)**

```yaml
services:
  immich-upload-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-upload-optimizer:latest
    ports:
      - "2283:2283"
    environment:
      - IUO_UPSTREAM=http://immich-server:2283
    volumes:
      - ./config:/etc/immich-upload-optimizer/config   # only if you use your own tasks
    depends_on:
      - immich-server
```

**After (immich-optimizer)**

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    ports:
      - "2283:2283"
    environment:
      - IO_MODE=proxy
      - IO_IMMICH_URL=http://immich-server:2283
    volumes:
      - ./undone:/undone                               # failed uploads are kept here
      - ./config:/etc/immich-optimizer/config          # only if you use your own tasks
    depends_on:
      - immich-server
    restart: always
```

What changed:

1. **Image name**: `ghcr.io/miguelangel-nubla/immich-optimizer`.
2. **Mode**: set `IO_MODE=proxy`. There is no default: without a mode the optimizer does not start. Proxy mode needs no API key: it forwards each client's own credentials.
3. **Environment variables and flags**: the old names are gone and are not read any more. A variable left at its old name is silently ignored, and an old flag stops the optimizer at startup. Rename each one you use:

   | Old name | New name |
   |---|---|
   | `IUO_MODE` | `IO_MODE` |
   | `IUO_LOG_LEVEL` | `IO_LOG_LEVEL` |
   | `IUO_IMMICH_URL`, `IUO_UPSTREAM`, `-upstream` | `IO_IMMICH_URL`, `-immich_url` |
   | `IUO_IMMICH_API_KEY` | `IO_IMMICH_API_KEY` |
   | `IUO_IMMICH_API_KEY_FILE` | `IO_IMMICH_API_KEY_FILE` |
   | `IUO_HTTP_TIMEOUT_SECONDS`, `-http_timeout_seconds` | `IO_IMMICH_TIMEOUT`, `-immich_timeout` (a [duration](configuration.md#durations) such as `90s`, not a number of seconds) |
   | `IUO_TASKS_FILE` | `IO_TASKS_FILE` |
   | `IUO_MAX_CONCURRENT_REQUESTS`, `-max_concurrent_requests` | `IO_TASKS_CONCURRENCY`, `-tasks_concurrency` |
   | `IUO_WATCHER_UNDONE_DIR`, `IUO_UNDONE_DIR` | `IO_UNDONE_DIR` |
   | `IUO_PROXY_BIND_ADDR`, `IUO_BIND_ADDR`, `IUO_LISTEN`, `-bind_addr`, `-listen` | `IO_PROXY_BIND_ADDR`, `-proxy_bind_addr` |
   | `IUO_WATCHER_WATCH_DIR`, `IUO_WATCH_DIR`, `-watch_dir` | `IO_WATCHER_DIR`, `-watcher_dir` |
   | `IUO_WATCHER_INOTIFY_BUFFER_SIZE`, `IUO_INOTIFY_BUFFER_SIZE`, `-inotify_buffer_size` | `IO_WATCHER_INOTIFY_BUFFER_SIZE`, `-watcher_inotify_buffer_size` |

   `IUO_FILTER_PATH` and `IUO_FILTER_FORM_KEY` have no replacement: the proxy always converts uploads to Immich's upload endpoint. See [Configuration](configuration.md) for every setting.
4. **Port**: unchanged. The proxy listens on `:2283`, so a `2283:2283` mapping keeps working.
5. **Config folder**: mount your tasks at `/etc/immich-optimizer/config` (was `/etc/immich-upload-optimizer/config`). A mount left at the old path is ignored without any error and the default lossless profile runs instead.
6. **Undone folder**: mount `/undone`. When a conversion fails, or no task matches, the client still gets an error and nothing is uploaded, and the original is now kept there. See [Undone folder](undone.md).
7. **Container user**: the container no longer runs as root. See [Folders and ports](docker.md#folders-and-ports).
8. **Custom `tasks.yaml`**: the input and the output now live in separate folders.
   - Replace `{{.folder}}` with `{{.src_folder}}` for the input and `{{.dst_folder}}` for the output. A task that still uses `{{.folder}}` stops the optimizer at startup with a template error.
   - A command must leave **exactly one file** in `{{.dst_folder}}`. Tools that used to rewrite the file in place must now write there, and scripts that skip a file must copy it there.
   - Remove `-new` suffixes and trailing `rm` commands that only existed to work in a single folder.

   ```yaml
   # Old (immich-upload-optimizer):
   command: cjxl --lossless_jpeg=1 {{.folder}}/{{.name}}.{{.extension}} {{.folder}}/{{.name}}.jxl

   # New (immich-optimizer):
   command: cjxl --lossless_jpeg=1 "{{.src_folder}}/{{.name}}.{{.extension}}" "{{.dst_folder}}/{{.name}}.jxl"
   ```
9. **Motion photos**: the bundled profiles now keep the photo and drop the embedded video. See [Motion photos](tasks.md#motion-photos) to change that.
10. **Long conversions**: a conversion is no longer cut off when the client disconnects or a timeout passes. It finishes and the file is uploaded.
11. **Error codes**: an unmatched file type answers `415` and a failed conversion answers `502` (both were `500`).
12. **GPU**: no bundled task uses a graphics card. If you used one, start from the [custom image template](docker.md#custom-image).
13. **Downloads**: with the bundled profiles, an original stored as JPEG XL is now downloaded as JPEG. Remove the `downloads` list from your tasks file to get the stored file. See [Downloads](tasks.md#downloads).
14. **Health check**: the image's health check now asks the running proxy instead of only starting the program. See [Health check](docker.md#health-check).

If you use the official mobile app: it no longer uploads converted photos again and again. See [The mobile app](proxy.md#the-mobile-app).
