# Undone folder (failed files)

When a file cannot be converted, **the original is never uploaded in its place.** It is saved to the undone folder (`IO_UNDONE_DIR`, `/undone` in the image) with a note saying why, and you decide what to do with it.

This is the same in proxy and watcher mode. Library mode does not use the folder: its originals simply stay in Immich.

## When a file ends up there

- Every task for its type failed, or produced an empty file.
- A task ran longer than the [task timeout](configuration.md#tasks).
- No task handles its type (unless `IO_UNDONE_UNMATCHED=false`).
- In watcher mode only: the file was converted, but Immich did not accept it, for example because the quota is full.

Each save is also recorded in the [audit log](audit-log.md).

**What the sender sees**

- **Proxy mode:** the client gets an error (`415` when no task handles the type, `502` when the conversion failed) and still has its file. The mobile app tries again by itself each time it looks for things to back up. A failed upload *to Immich* (network trouble, an Immich error) is not saved here: the client still has the file and retries, so saving would only pile up copies.
- **Watcher mode:** the file leaves the watch folder only once it has been uploaded or saved here. If saving fails too, for example because the disk is full, it stays in the watch folder.

## What to do with a file

Each failed file has its own folder:

```text
undone/
  3f8a…c1/
    original.file          the file, exactly as it arrived, with its own date
    metadata.json          its name, where it came from, and what went wrong
```

1. **Find out why.** Open `metadata.json`: `failures` lists each attempt with the command that ran and the end of its output.
2. **Fix the cause**, usually a task in your [tasks file](tasks.md).
3. **Send it again.**
   - Proxy mode: upload it again from the client.
   - Watcher mode: copy `original.file` into the watch folder under its original name, which is in `metadata.json` (`original_filenames`). The name matters because its extension selects the task:

     ```sh
     cp undone/3f8a…c1/original.file /watch/IMG_1234.HEIC
     ```
4. **Delete the folder** once the file is in Immich.

## Details

### How files are stored

**The folder name** is the SHA-1 checksum of the file (40 characters), the same checksum Immich keeps for every asset.

- The same file failing again is not saved twice: `occurrences` goes up and the new failure is added to its `metadata.json`. This is what happens when a phone retries an upload.
- A different file with the same name gets its own folder, so nothing is ever overwritten.
- The file is always called `original.file`, with no extension, so an unusual or very long name can never cause trouble. The real name is in `metadata.json`.
- There are no subfolders per mode, and the watch folder's tree is not copied. Where a file came from is in `metadata.json`.

### `metadata.json`

| Field | Meaning |
|---|---|
| `sha1`, `sha1_base64` | the checksum, as the folder name and as Immich's API shows it |
| `size`, `original_mtime` | the file's size and its own date |
| `first_failed_at`, `last_failed_at`, `occurrences` | when and how often it failed |
| `original_filenames` | the names it arrived under (up to 100) |
| `failures` | the 20 most recent failures |

Each failure has:

| Field | Meaning |
|---|---|
| `source` | `watcher` or `proxy` |
| `original_filename`, `original_dir` | the name, and the path below the watch folder or the folder part of the name the proxy's client sent |
| `failed_at` | when |
| `stage` | `no_task`, `convert` or `upload` |
| `error` | what went wrong |
| the tasks that were tried | for each: command, exit code, duration, whether it timed out, and the last 4 KiB of its output |

It holds no passwords, keys or request headers. The format is `"version": 3`.

### Checking whether Immich already has a file

Since the folder name is the checksum Immich uses, you can ask Immich for it. This only finds the file if these exact bytes reached Immich some other way, for example from a client that did not go through the proxy; a converted upload has another checksum.

```sh
curl -s -X POST "$IMMICH_URL/api/assets/bulk-upload-check" \
  -H "x-api-key: $IMMICH_API_KEY" -H 'Content-Type: application/json' \
  -d '{"assets":[{"id":"x","checksum":"<folder name>"}]}'
# {"results":[{"id":"x","action":"reject","reason":"duplicate","assetId":"…"}]} means Immich has it
```

### Other things to know

**Several containers can share one undone folder**, for example a watcher and a proxy. They take turns through a lock file, `.io-undone.lock`, at its top: leave it there.

**Unfinished saves.** A folder only appears once it is complete. Entries named `.io-undone-*` are saves that a crash interrupted; they are removed at the next start, once they are over an hour old.

**Permissions.** Files are written by the container's user (id `1001`). The folder must be writable by it, which is checked at startup.

**Folders from earlier versions** have 64-character names (a SHA-256 checksum, with `"sha256"` in their `metadata.json`). They are left exactly as they are. If the same file fails again it gets a new folder next to the old one. Retry or delete the old ones by hand.
