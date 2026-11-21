# Provenance marker

For every file it converts, the optimizer stores a record in Immich's asset metadata under the key `immich-optimizer`. It lists the **versions** of the file, oldest first: the original, then each conversion, with exactly how it was made. The last version is the file as it is in Immich.

```json
{
  "v": 1,
  "versions": [
    { "sha1": "86e66ce4…", "size": 37993, "name": "IMG_1234.jpg" },
    {
      "sha1": "2b1f9a07…", "size": 21961, "name": "IMG_1234.jxl",
      "at": "2026-11-21T10:00:00Z",
      "source": "proxy",
      "optimizer": "v0.4.0",
      "task": {
        "name": "jpeg-xl",
        "command": "cjxl --lossless_jpeg=1 -d 0 \"{{.src_folder}}/{{.name}}.{{.extension}}\" \"{{.dst_folder}}/{{.name}}.jxl\"",
        "motion_photos": "drop_video"
      },
      "config": { "sha256": "9f2c…" }
    }
  ],
  "library": { "replaces": "2f908a71-…", "state": "pending" }
}
```

**The first version is the original**, as it was before any conversion: its `sha1` is the checksum Immich would have stored for it, the one phones still have. **Each later version was made from the one before it.** A file converted once has two versions; one [converted again](library.md#converting-again) gets one more each time, and its first version stays the same.

| Field | Meaning |
|---|---|
| `versions[].sha1` | SHA-1 of that version, Immich's checksum for it |
| `versions[].size` | its size in bytes |
| `versions[].name` | its file name |
| `versions[].at` | when it was made (every version but the first) |
| `versions[].source` | what made it: `proxy`, `watcher` or `library` |
| `versions[].optimizer` | the optimizer version |
| `versions[].task.name` | the task that made it |
| `versions[].task.command` | that task's command **as it was in the tasks file at the time**, placeholders included. A task keeps its name when you change what it does; this is what was actually run |
| `versions[].task.motion_photos` | `drop_video` or `drop_photo`, only when the file was a motion photo that was split |
| `versions[].config.sha256` | a fingerprint of the folder the tasks file is in: the file itself and the scripts and presets next to it. Two versions with the same value were made with the same configuration; a changed script changes it even when the command is the same |
| `library.replaces` | the id of the asset this one replaced (library mode) |
| `library.state` | `pending` while library mode is still replacing the earlier asset with this one; absent once it is done |
| `kept` | on an asset library mode left as it is, instead of any conversion: `note` says why (for example, converting it did not make it smaller), with `at` and `optimizer`. Its `versions` holds only the original |

A part that does not apply is left out.

- It is written with the upload itself, next to any metadata the client sent (the mobile app's own item is kept).
- Files uploaded unchanged get no marker.
- When Immich answers "duplicate", the marker is added to the existing asset if it has none. The proxy also writes an existing marker again, unchanged, so Immich sends it to phones once more. A marker the optimizer cannot read (one written before versions, for example) counts as none and is replaced.
- The optimizer keeps no database: this record inside Immich is the only state.
- It is what the proxy's [app sync](app-sync.md) tells the mobile app the original's checksum from, and how library mode recognises [second copies](library.md#second-copies-of-converted-originals) and what it has already handled.
- The command is stored as written, so do not put a password or a token into a task's command. Anyone who can see the asset in Immich can read its metadata.
- Read it with `GET /api/assets/<id>/metadata`.
