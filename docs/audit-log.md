# Audit log

Every operation on an asset is appended to `audit.log` in the undone folder (change the path with `IO_AUDIT_LOG_FILE`), in every mode. It is separate from the normal log output: it is persistent, it has one JSON object per line, and each line is written to disk before the optimizer goes on.

It records what happened to your files, and only that:

| Source | Event | Meaning |
|---|---|---|
| `proxy`, `watcher` | `uploaded` | a file reached Immich; `outcome` says whether it was `optimized` or sent `unchanged` |
| `proxy`, `watcher` | `duplicate` | Immich already had those bytes |
| `proxy`, `watcher` | `failed` | conversion failed; `undone` is the folder the original was saved to |
| `proxy`, `watcher` | `refused` | no task handles the file type |
| `proxy`, `watcher` | `upload_failed` | converted, but Immich did not accept it |
| `library` | `would_convert` | dry run: this asset would be converted |
| `library` | `converted`, `uploaded`, `verified`, `replaced` | the steps of one replacement; `replaced` means the original is in the trash |
| `library` | `kept` | looked at and left as it is |
| `library` | `verify_failed`, `rolled_back`, `failed` | a replacement was abandoned and the original left untouched; `differences` lists what did not match |
| `library` | `finalized` | an interrupted replacement from an earlier run was completed |
| `library` | `run_started`, `run_finished` | one pass, with a summary of counts and bytes |

Requests that are only passed through to Immich, malformed uploads and other client mistakes are not recorded.

```json
{"time":"2026-11-20T12:28:41Z","run":"20261120T122839Z","source":"library","event":"replaced","asset":"2f908a71-…","file":"IMG_1234.jpg","new_asset":"552cdf2a-…","original_bytes":29293,"converted_bytes":21961,"original_sha1":"6ea53f87…","converted_sha1":"1df45779…","task":"jpeg-xl"}
```

Each line carries the time, the run it belongs to, the file name, the Immich asset id, and where known the task, both sizes and both checksums.

If the audit log cannot be written, **library mode stops before changing anything**. Proxy and watcher mode log a warning and keep working, so a full disk does not block uploads.
