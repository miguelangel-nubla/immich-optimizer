# Provenance marker

For every file it converts, the optimizer stores a small record in Immich's asset metadata under the key `immich-optimizer`:

```json
{"v": 1, "sha1": "86e66ce4…", "size": 37993, "task": "jpeg-xl", "source": "proxy", "optimizer": "v0.3.0"}
```

| Field | Meaning |
|---|---|
| `sha1` | SHA-1 of the original file, the checksum Immich would have stored for it |
| `size` | size of the original in bytes |
| `task` | the task that produced the uploaded file |
| `source` | `proxy`, `watcher` or `library` |
| `replaces` | the id of the asset this one replaced (library mode) |
| `kept` | `true` on an original that library mode looked at and left as it is, with the reason in `note` |
| `state` | `pending` while library mode is still replacing the original with this asset; absent once it is done |
| `motion_photos` | `drop_video` or `drop_photo`, only when the original was a motion photo that was split |
| `optimizer` | the optimizer version |

- It is written with the upload itself, next to any metadata the client sent (the mobile app's own item is kept).
- Files uploaded unchanged get no marker.
- When Immich answers "duplicate", the marker is added to the existing asset if it has none. With the proxy's app sync on, an existing marker is written again unchanged, so Immich sends it to phones once more.
- The optimizer keeps no database: this record inside Immich is the only state.
- It is what the proxy's [app sync](proxy.md#experimental-making-the-mobile-app-see-converted-photos-as-backed-up) tells the mobile app the original's checksum from, and how library mode recognises [second copies](library.md#second-copies-of-converted-originals) and what it has already handled.
- Read it with `GET /api/assets/<id>/metadata`.
