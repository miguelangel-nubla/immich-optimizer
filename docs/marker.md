# Provenance marker

For every file it converts, the optimizer stores a record in Immich's asset metadata under the key `immich-optimizer`. It says what the original was and exactly how it was converted:

```json
{
  "v": 1,
  "original": {
    "sha1": "86e66ce4…",
    "size": 37993,
    "name": "IMG_1234.jpg"
  },
  "conversion": {
    "at": "2026-11-21T10:00:00Z",
    "source": "proxy",
    "optimizer": "v0.3.0",
    "task": {
      "name": "jpeg-xl",
      "command": "cjxl --lossless_jpeg=1 -d 0 \"{{.src_folder}}/{{.name}}.{{.extension}}\" \"{{.dst_folder}}/{{.name}}.jxl\"",
      "motion_photos": "drop_video"
    },
    "config": { "sha256": "9f2c…" },
    "output": { "size": 21961 }
  },
  "library": { "replaces": "2f908a71-…", "state": "pending" },
  "kept": { "note": "the conversion was not smaller" }
}
```

| Field | Meaning |
|---|---|
| `original.sha1` | SHA-1 of the original file, the checksum Immich would have stored for it |
| `original.size` | size of the original in bytes |
| `original.name` | the original's file name |
| `conversion.at` | when it was converted |
| `conversion.source` | `proxy`, `watcher` or `library` |
| `conversion.optimizer` | the optimizer version |
| `conversion.task.name` | the task that produced the stored file |
| `conversion.task.command` | that task's command **as it was in the tasks file at the time**, placeholders included. A task keeps its name when you change what it does; this is what was actually run |
| `conversion.task.motion_photos` | `drop_video` or `drop_photo`, only when the original was a motion photo that was split |
| `conversion.config.sha256` | a fingerprint of the folder the tasks file is in: the file itself and the scripts and presets next to it. Two assets with the same value were converted with the same configuration; a changed script changes it even when the command is the same |
| `conversion.output.size` | size of the converted file in bytes |
| `library.replaces` | the id of the asset this one replaced (library mode) |
| `library.state` | `pending` while library mode is still replacing the original with this asset; absent once it is done |
| `kept` | present on an original that library mode looked at and left as it is, with the reason in `note`. Such a record has no `task` |

Only `v` and `original` are always there; a part that does not apply is left out.

- It is written with the upload itself, next to any metadata the client sent (the mobile app's own item is kept).
- Files uploaded unchanged get no marker.
- When Immich answers "duplicate", the marker is added to the existing asset if it has none. The proxy also writes an existing marker again, unchanged, so Immich sends it to phones once more.
- The optimizer keeps no database: this record inside Immich is the only state.
- It is what the proxy's [app sync](app-sync.md) tells the mobile app the original's checksum from, and how library mode recognises [second copies](library.md#second-copies-of-converted-originals) and what it has already handled.
- The command is stored as written, so do not put a password or a token into a task's command. Anyone who can see the asset in Immich can read its metadata.
- Read it with `GET /api/assets/<id>/metadata`.
