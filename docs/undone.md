# Undone folder (failed files)

The undone folder works the same in proxy and watcher mode. **The original is never uploaded when conversion fails.** Instead it is saved to `IO_UNDONE_DIR` together with a `metadata.json` explaining why. Each save is also recorded in the [audit log](audit-log.md).

Library mode does not use it: its originals stay in Immich.

- **When a file is saved:** every task for its extension failed, a task produced an empty output, the [task timeout](configuration.md#tasks) expired, or no task matches its extension (unless `IO_UNDONE_UNMATCHED=false`). In watcher mode, a file whose upload to Immich fails (for example quota exceeded) is also saved.
- **Layout:** flat and content-addressed, one folder per distinct file content, named after its SHA-1 (the same checksum Immich stores for every asset):

  ```
  undone/
    3f8a…c1/                 SHA-1 (40 hex characters) of the file content
      original.file          the original, byte for byte, with its original modification time
      metadata.json          names, sources, folders and failures
  ```

  `original.file` is literally named that (no extension), so unusual or very long client filenames can never cause problems. There are no per-source subfolders and the watch directory's tree is not mirrored: the source and the original filename and folder are recorded in `metadata.json`. A folder only appears once it is complete (it is assembled under a temporary name and renamed into place); temporary `.io-undone-*` entries left by a crash are removed at startup (leftover copies once they are over an hour old). The undone directory may be shared by several optimizer processes, for example a watcher and a proxy container mounting the same folder: they coordinate through an advisory lock file, `.io-undone.lock`, at its root (leave it in place).
- **metadata.json** (pretty-printed JSON, `"version": 3`, written atomically) contains `sha1` (hex, the folder name), `sha1_base64` (the same checksum the way the Immich API shows it), `size`, `original_mtime`, `first_failed_at`, `last_failed_at`, `occurrences`, `original_filenames` (the distinct names this content arrived under, up to 100) and `failures`: the 20 most recent failures, each with `source` (`watcher` / `proxy`), `original_filename`, `original_dir` (the watcher's path relative to the watch directory, or the folder part of the proxy client's filename if it sent one), `failed_at`, `stage` (`no_task`, `convert` or `upload`), `error`, and every task attempted (command, exit code, duration, timeout flag, last 4 KiB of output). It contains no credentials or headers.
- **No duplicates:** if the identical content is already there (for example a phone retrying the same upload, or the same photo arriving from both the watcher and the proxy), it is not copied again (it does not even need temporary space); `occurrences` goes up and the failure is appended to its `metadata.json`. A *different* file with the same name always gets its own folder, so nothing is ever overwritten.
- **Finding a file in Immich:** Immich identifies every asset by the SHA-1 of its file, so the folder name is directly comparable with Immich's `checksum`. This only matches if these exact original bytes were uploaded to Immich some other way (for example from a client that bypassed the proxy), since an optimized upload has a different checksum. To check before retrying, ask Immich's API with the hex folder name or `sha1_base64`:

  ```sh
  curl -s -X POST "$IMMICH_URL/api/assets/bulk-upload-check" \
    -H "x-api-key: $IMMICH_API_KEY" -H 'Content-Type: application/json' \
    -d '{"assets":[{"id":"x","checksum":"<folder name>"}]}'
  # {"results":[{"id":"x","action":"reject","reason":"duplicate","assetId":"…"}]} means Immich already has it
  ```

  or `POST /api/search/metadata` with `{"checksum":"<sha1_base64>"}`.
- **Folders from earlier versions:** before metadata version 3, folders were named after the SHA-256 of the content (64 hex characters, `"sha256"` in `metadata.json`). They are left exactly as they are: never updated, cleaned up or deduplicated against. If the same content fails again it gets a new SHA-1 folder next to the old one. Retry or delete old folders by hand.
- **Watcher:** the source file is removed from the watch directory only once it has been uploaded or saved to undone. If saving fails (for example the disk is full), the file stays in the watch directory.
- **Proxy:** the client still gets an error (`415` for an unmatched extension, `502` for a conversion failure) and keeps its original, so it can retry. The mobile app does so by itself each time it looks for things to back up. Upload failures to Immich (network errors, Immich `5xx`) are **not** saved in proxy mode: the client keeps the original and retries, so saving would only pile up copies.
- **Retrying a file:** fix the cause (for example the task), then copy `original.file` into the watch directory under its original name from `metadata.json` (`original_filenames`, or `original_filename` of a failure; the extension selects the task), for example `cp undone/3f8a…c1/original.file /watch/IMG_1234.HEIC`. Once it is uploaded you can delete the hash folder. In proxy mode you can instead just retry the upload from the client.
- **Permissions:** files are created by the container user (`appuser`, uid 1001) with the default umask. The directory must be writable by that user; this is checked at startup.
