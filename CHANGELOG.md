# Changelog

What changes for someone who runs the optimizer: behaviour, settings, files. The release notes on GitHub list every commit as well.

## Unreleased

### Fixed

- **`profile1` replaced DNG files with their thumbnail.** The tools in the image cannot develop camera RAW; given a DNG they read the small preview inside it (160x120 for the one this was found with), and that valid, much smaller JPEG XL replaced the photo. DNG is now uploaded as it is, like every other RAW format. If you used `profile1` with DNG files, look for `.jxl` assets of a few kilobytes whose original name ended in `.dng`: with library mode their originals are in Immich's trash until it is emptied.
- **TIFF files were never uploaded.** `caesiumclt` does not take TIFF, and no other task did, so every `.tif` and `.tiff` went to the undone folder. Both profiles now upload TIFF as it is.
- **A result tiny next to its original is refused**, in every mode: less than 1/16 of the width and of the height. Such a result is the preview inside a file, not the photo; the original is kept (undone folder, or left in Immich).
- **Watcher mode**
  - Files found by a scan (at startup, or in a folder copied or moved in) are taken only once they have not been written to for 10 seconds; before, a file still being copied could be uploaded truncated and then deleted.
  - A file overwritten while it was being converted is kept and handled again, instead of being deleted unconverted.
  - A subfolder that cannot be read, or the limit of inotify watches, no longer leaves the folders after it unwatched.
  - A file published with a hard link could be missed on a busy machine; it is now taken once it has settled.
- **Proxy mode**
  - A request with a full address in its request line was sent to that address, with the client's credentials. Requests now always go to `IO_IMMICH_URL`.
  - An upload no longer forwards the client's `X-Immich-Checksum`, which belongs to the unconverted file.
  - The app is not told a photo is safely backed up when its converted copy is in Immich's trash and only the marker went by.
  - A player or download manager that asks for one converted download in many pieces has it fetched and converted once, not once per piece.
- **Library mode**
  - A second copy was trashed against another user's conversion that this user could see through partner sharing or a shared album.
  - When Immich applied the move to the trash but its answer was lost, the verified copy was deleted, leaving the photo only in the trash.
  - Uploads made while library mode was not listening (stopped, disconnected, or a burst) are now checked for second copies.
  - After a failed write of the audit log, library mode stopped for good; it now goes on once the log can be written again.
  - Second copies are found also when the default tasks file has no task for their type.

### Changed

- **Settings that cannot be read are refused at startup.** `IO_PROXY_CONVERT_UPLOADS=maybe` used to mean `false`, and `IO_TASKS_CONCURRENCY=abc` 0. On/off settings take `true`/`false`, `yes`/`no`, `on`/`off` or `1`/`0`. An unknown `IO_LOG_LEVEL` is refused instead of meaning `info`.
- **A task that exits 0 without leaving exactly one file with content** counts as failed, so the next matching task is tried.
- On shutdown with several modes, proxy and watcher stop taking work at once, and all modes share one `IO_SHUTDOWN_TIMEOUT`.
- Library mode reads the whole library with Immich's current search instead of the one Immich will remove in version 4.
- Built with Go 1.26.11 (security fixes in the standard library).
- **Only the current Immich (3.x) is supported.** Uploads no longer send `deviceAssetId` and `deviceId`, which Immich 1.x and 2.x required and 3.x ignores.
