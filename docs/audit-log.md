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
| `library` | `converted`, `uploaded`, `verified`, `replaced` | the steps of one replacement; `replaced` means the original is in the trash. For a photo converted again (tagged `convert` while already converted), `detail` says so and `original_sha1` is still the first original's |
| `library` | `kept` | looked at and left as it is |
| `library` | `verify_failed`, `rolled_back`, `failed` | a replacement was abandoned and the original left untouched; `differences` lists what did not match |
| `library` | `second_copy_trashed` | an original that was uploaded again after its conversion was moved to the trash; `new_asset` is the conversion. |
| `library` | `finalized` | an interrupted replacement from an earlier run was completed |
| `library` | `run_started`, `run_finished` | one batch of work (what was tagged, or an upload to check), with a summary of counts and bytes |
| `library` | `tag_added`, `tag_removed` | a tag (`detail`) was added to or removed from an asset: `convert` removed once it was acted on, `failed` or `review` added |
| `library` | `tag_created`, `tag_deleted` | one of the optimizer's tags (`detail`) was created, or deleted because the optimizer no longer uses it |
| `library` | `full_read` | the whole library was read, because the audit log had no earlier record to go by |

Requests that are only passed through to Immich, malformed uploads and other client mistakes are not recorded.

```json
{"time":"2026-11-20T12:28:41Z","run":"20261120T122839Z","source":"library","event":"replaced","asset":"2f908a71-…","file":"IMG_1234.jpg","new_asset":"552cdf2a-…","original_bytes":29293,"converted_bytes":21961,"original_sha1":"6ea53f87…","converted_sha1":"1df45779…","task":"jpeg-xl"}
```

Each line carries the time, the run it belongs to, the file name, the Immich asset id, and where known the task, both sizes and both checksums.

**The audit log is mandatory.** The optimizer writes a `started` line when it starts and refuses to start if it cannot. If a later write fails (a full disk, a lost mount), nothing new is converted or uploaded until the log can be written again: the proxy answers `503` so clients retry, the watcher leaves files in the watch folder, and library mode stops its run. It recovers by itself, and marks the gap with an `audit_recovered` line.
