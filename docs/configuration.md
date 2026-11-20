# Configuration

Every setting can be given as an environment variable or as a command-line flag. The flag is the variable's name without `IO_`, in lower case: `IO_PROXY_BIND_ADDR` is `-proxy_bind_addr`.

- A flag wins over an environment variable.
- A setting's name starts with what it belongs to: `IO_PROXY_…` only matters in proxy mode, `IO_TASKS_…` is about converting files. Settings without such a prefix apply to everything.
- `immich-optimizer -help` prints this list.

What to convert, and how, is not a setting: it is in the [tasks file](tasks.md).

## Settings

<!-- BEGIN SETTINGS: generated from cmd/optimizer/settings.go, do not edit. Run: IO_UPDATE_DOCS=1 go test ./cmd/optimizer -run TestSettingsDocs -->

### General

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `IO_MODE` | `-mode` | `watcher` | What to run: `watcher`, `proxy`, `library`, or several separated by commas |
| `IO_LOG_LEVEL` | `-log_level` | `info` | How much to log: `debug`, `info`, `warn` or `error` |
| `IO_SHUTDOWN_TIMEOUT` | `-shutdown_timeout` | `8s` | On shutdown, how long running jobs get to finish before they are cancelled. A [duration](#durations) |

### Immich

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `IO_IMMICH_URL` | `-immich_url` |  | Address of the Immich server, for example `http://immich-server:2283`. Required |
| `IO_IMMICH_API_KEY` | `-immich_api_key` |  | Immich API key. Required in watcher and library mode; not used by proxy mode |
| `IO_IMMICH_API_KEY_FILE` | `-immich_api_key_file` |  | File that contains the API key, for example a Docker secret. Wins over `immich_api_key` |
| `IO_IMMICH_TIMEOUT` | `-immich_timeout` | `2m` | How long to wait when connecting to Immich, for an upload that stops making progress, and for Immich's answer after an upload. It does not limit how long an upload may take in total. A [duration](#durations) |

### Tasks

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `IO_TASKS_FILE` | `-tasks_file` | `./tasks.yaml`, else `/etc/immich-optimizer/config/tasks.yaml` | The tasks file: which command converts which file type |
| `IO_TASKS_CONCURRENCY` | `-tasks_concurrency` | `10` | How many files are converted at the same time, counted across all modes |
| `IO_TASKS_TIMEOUT` | `-tasks_timeout` | `0` | Longest time one file may take to convert; `0` means no limit. A [duration](#durations) |

### Undone folder and audit log

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `IO_UNDONE_DIR` | `-undone_dir` | `/undone` | Folder where files that could not be converted are kept, in every mode |
| `IO_UNDONE_UNMATCHED` | `-undone_unmatched` | `true` | Also keep files whose type no task handles (they are refused either way) |
| `IO_AUDIT_LOG_FILE` | `-audit_log_file` | `audit.log` in the undone folder | Persistent record of every asset operation |

### Proxy mode

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `IO_PROXY_BIND_ADDR` | `-proxy_bind_addr` | `:2283` | Address and port the proxy listens on |
| `IO_PROXY_FILTER_PATH` | `-proxy_filter_path` | `/api/assets` | Request path whose uploads are converted |
| `IO_PROXY_FILTER_FORM_KEY` | `-proxy_filter_form_key` | `assetData` | Name of the form field that carries the file |

### Watcher mode

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `IO_WATCHER_DIR` | `-watcher_dir` | `/watch` | Folder to watch for new files |
| `IO_WATCHER_INOTIFY_BUFFER_SIZE` | `-watcher_inotify_buffer_size` | `8192` | Size in bytes of the buffer for file system events |

### Library mode

| Environment variable | Flag | Default | Description |
|---|---|---|---|
| `IO_LIBRARY_DRY_RUN` | `-library_dry_run` | `true` | Only report what would be converted and change nothing |
| `IO_LIBRARY_TAKEN_BEFORE` | `-library_taken_before` |  | Only convert assets dated before this day (`YYYY-MM-DD`) |
| `IO_LIBRARY_LIMIT` | `-library_limit` | `0` | Stop after this many replacements; `0` means no limit |
| `IO_LIBRARY_CONCURRENCY` | `-library_concurrency` | `1` | How many assets are replaced at the same time. Each one makes Immich generate thumbnails and analyse a new file |
| `IO_LIBRARY_READY_TIMEOUT` | `-library_ready_timeout` | `10m` | How long to wait for Immich to finish reading a converted asset. A [duration](#durations) |

<!-- END SETTINGS -->

## Durations

Settings that hold a length of time take a number with a unit: `90s`, `15m`, `1h30m`. The units are `s`, `m` and `h` (also `ms`). A number without a unit is refused, except `0`.

## Old names

These names from earlier versions and from `immich-upload-optimizer` still work. Each one in use is listed in a warning at startup; they will be removed in a future release. Old names of duration settings take whole seconds, as they always did. If both the old and the new name are set, the new one wins.

<!-- BEGIN OLD NAMES: generated, do not edit -->

| Old name | Use instead |
|---|---|
| `IUO_MODE` | `IO_MODE`, `-mode` |
| `IUO_LOG_LEVEL` | `IO_LOG_LEVEL`, `-log_level` |
| `IUO_IMMICH_URL`, `IUO_UPSTREAM`, `-upstream` | `IO_IMMICH_URL`, `-immich_url` |
| `IUO_IMMICH_API_KEY` | `IO_IMMICH_API_KEY`, `-immich_api_key` |
| `IUO_IMMICH_API_KEY_FILE` | `IO_IMMICH_API_KEY_FILE`, `-immich_api_key_file` |
| `IUO_HTTP_TIMEOUT_SECONDS`, `-http_timeout_seconds` | `IO_IMMICH_TIMEOUT`, `-immich_timeout` |
| `IUO_TASKS_FILE` | `IO_TASKS_FILE`, `-tasks_file` |
| `IUO_MAX_CONCURRENT_REQUESTS`, `-max_concurrent_requests` | `IO_TASKS_CONCURRENCY`, `-tasks_concurrency` |
| `IUO_WATCHER_UNDONE_DIR`, `IUO_UNDONE_DIR` | `IO_UNDONE_DIR`, `-undone_dir` |
| `IUO_PROXY_BIND_ADDR`, `IUO_BIND_ADDR`, `IUO_LISTEN`, `-bind_addr`, `-listen` | `IO_PROXY_BIND_ADDR`, `-proxy_bind_addr` |
| `IUO_PROXY_FILTER_PATH`, `IUO_FILTER_PATH`, `-filter_path` | `IO_PROXY_FILTER_PATH`, `-proxy_filter_path` |
| `IUO_PROXY_FILTER_FORM_KEY`, `IUO_FILTER_FORM_KEY`, `-filter_form_key` | `IO_PROXY_FILTER_FORM_KEY`, `-proxy_filter_form_key` |
| `IUO_WATCHER_WATCH_DIR`, `IUO_WATCH_DIR`, `-watch_dir` | `IO_WATCHER_DIR`, `-watcher_dir` |
| `IUO_WATCHER_INOTIFY_BUFFER_SIZE`, `IUO_INOTIFY_BUFFER_SIZE`, `-inotify_buffer_size` | `IO_WATCHER_INOTIFY_BUFFER_SIZE`, `-watcher_inotify_buffer_size` |

<!-- END OLD NAMES -->
