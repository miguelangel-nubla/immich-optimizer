# Changelog

What changes for someone who runs the optimizer: behaviour, settings, files. The release notes on GitHub list every commit as well.

## Unreleased

- **Watch folders take files again from SFTP clients that keep the capture time.** Since 0.4.0, a file uploaded under a temporary name whose modification time was set back before it was published with a hard link (an Android sync app's `.tacitpart` files, for example) was never taken: it was only picked up by the folder scan when the optimizer restarted. It is now taken once it has not been written to for ten seconds.

## 0.4.1 (2026-11-22)

A bug fix for uploads from the web app, and new documentation on video tasks. Nothing to change when upgrading from 0.4.0.

- **A long upload from the web app no longer times out behind a reverse proxy.** The keep-alive that sends a browser from one short wait to the next never applied to the Immich web app: it was given only to requests asking for a web page. Browsers are now recognised by a header every browser adds and scripts cannot set (`Sec-Fetch-Dest`), and the redirect is a `303`, so the browser no longer sends the whole file again at each step. Browsers send that header over HTTPS only, which is how a web app behind such a reverse proxy is opened. A new end-to-end test uploads from a real Chrome.
- **Docs:** [HandBrake or ffmpeg](docs/tasks.md#video-handbrake-or-ffmpeg) for video tasks, and how to keep a video's date and location with either: neither tool keeps them by itself.

## 0.4.0 (2026-11-22)

The first stable release of the 0.4 series, and the first one that moves `:latest` to it. The program is the same as 0.4.0-beta.1. The documentation was reorganised: library mode now has a second page, [How library mode works](docs/library-details.md).

**Upgrading from 0.3 or earlier, or from immich-upload-optimizer, is not a drop-in change.** Go through the [migration guide](docs/migration.md) before you update. The points that stop a container from starting, or change what it does (some were already in the 0.3 betas):

- **`IO_MODE` has to be set.** There is no default mode.
- **Settings are named `IO_...`.** The `IUO_...` names are not read any more: a variable left at its old name is silently ignored, and an old flag stops the optimizer at startup. Settings that hold a time are durations (`90s`), not a number of seconds.
- **The container runs as user id `1001`**, not root, and needs a writable folder mounted at `/undone`.
- **Tasks files:** `{{.folder}}` is gone. The input is in `{{.src_folder}}` and the command must leave exactly one file in `{{.dst_folder}}`.
- **Only the current Immich (3.x) is supported.**

What is new since 0.3, in short:

- **Library mode** converts the assets already in Immich. You choose them in Immich with the tag `immich-optimizer/convert`; each original goes to Immich's trash only after its converted copy has everything the original had. See [docs/library.md](docs/library.md).
- **The mobile app no longer uploads converted photos again.** The proxy tells the app the checksum of each original, without keeping a database. See [docs/proxy.md](docs/proxy.md#the-mobile-app).
- **Downloads are converted back**: with the bundled profiles, an original stored as JPEG XL is downloaded as JPEG.
- **A failed conversion never uploads the original in its place.** It is kept in the undone folder, with the reason.
- **A conversion is no longer cut off** when the client disconnects or a timeout passes.
- **Every converted asset carries a marker** in Immich that records its original, and every operation is written to a persistent audit log.
- **Motion photos and HDR photos** are handled by the `motion_photos` and `gain_maps` settings of the tasks file.

The entries below, and the release notes of the v0.4.0-dev releases, have the details.

## 0.4.0-beta.1 (2026-11-22)

Changes since v0.4.0-dev.7. For what changed from 0.3.0-beta.5 to dev.7, see the release notes of the v0.4.0-dev releases.

### Fixed

- **`profile1` replaced DNG files with their thumbnail.** The tools in the image cannot develop camera RAW; given a DNG they read the small preview inside it (160x120 for the one this was found with), and that valid, much smaller JPEG XL replaced the photo. DNG is now uploaded as it is, like every other RAW format. If you used `profile1` with DNG files, look for `.jxl` assets of a few kilobytes whose original name ended in `.dng`: with library mode their originals are in Immich's trash until it is emptied.
- **`profile1` could run commands hidden in a HEIC file's metadata.** Its `heic2jxl.sh` script compared the file's `MotionPhoto` value as a number, and bash evaluates such a value as an expression. A crafted upload could run commands as the optimizer's user. The script is gone (see Changed): HEIC motion photos are now split by the optimizer, which reads that value as data only.
- **Task commands no longer receive the Immich API key.** They inherited `IO_IMMICH_API_KEY` and `IO_IMMICH_API_KEY_FILE` with the rest of the optimizer's environment, and they are the programs that parse untrusted files.
- **In the Docker image, the program and its bundled configuration belong to root**, so a conversion tool that a crafted file takes over can no longer change them.
- **The default `IO_SHUTDOWN_TIMEOUT` is 5 seconds** (was 8), so that, with the time to cancel what still runs and answer proxy clients, shutdown fits Docker's default 10 second stop timeout.
- **A converted file is checked in every mode before it replaces the original**: it must decode completely as a photo or video and not be a single flat colour. Until now only library mode checked this; in proxy and watcher mode a task that exited 0 after writing a broken file (on a full disk, for example) had that file uploaded, and the watcher then deleted the original. Such a result now fails like any conversion, and the original goes to the undone folder.
- **What a task command leaves running in the background is stopped when the command exits.** Before, it kept running, could still be writing to the output that was being uploaded, and, if it kept the output open, made the conversion count as failed after 5 seconds.
- **Library mode stops the whole program, gracefully and with an error, if it fails**, also next to proxy or watcher mode. It used to log the failure and leave the rest running, looking healthy, with every tag ignored.
- A second Ctrl-C (or SIGTERM) during shutdown ends the program at once.
- **TIFF files were never uploaded.** `caesiumclt` does not take TIFF, and no other task did, so every `.tif` and `.tiff` went to the undone folder. Both profiles now upload TIFF as it is.
- **Samsung motion photos**
  - One UI 6 JPEG motion photos were not recognised: the size Samsung declares after the video (its own trailer) was read as coming before it. They were passed to the task whole, so `drop_video` and `drop_photo` did nothing and the video was lost in the conversion.
  - The video taken out of a One UI 5 JPEG ended with Samsung's trailer.
  - Found with the Samsung and Pixel samples Immich tests with, which are all split correctly now.
- **A result tiny next to its original is refused**, in every mode: less than 1/16 of the width and of the height. Such a result is the preview inside a file, not the photo; the original is kept (undone folder, or left in Immich).
- **Watcher mode**
  - Files found by a scan (at startup, or in a folder copied or moved in) are taken only once they have not been written to for 10 seconds; before, a file still being copied could be uploaded truncated and then deleted.
  - A file overwritten while it was being converted is kept and handled again, instead of being deleted unconverted.
  - A subfolder that cannot be read, or the limit of inotify watches, no longer leaves the folders after it unwatched.
  - A file published with a hard link could be missed on a busy machine; it is now taken once it has settled.
  - A file that was still open could be taken once it had not been written to for 10 seconds (a writer stalled on a network share), uploaded truncated, and deleted. A new file is now taken only when it is closed; only one published with a hard link is taken once it has settled.
  - A new file that got the inode number of a temporary file closed shortly before could be taken while still empty.
  - A file written to while it was being converted was still uploaded, with the old content (and a marker naming a checksum the file no longer had), before the change was noticed. It is now only handled again.
  - A backlog of many thousands of files (at startup) no longer starts one waiting handler per file at once.
- **Proxy mode**
  - A request with a full address in its request line was sent to that address, with the client's credentials. Requests now always go to `IO_IMMICH_URL`.
  - An upload no longer forwards the client's `X-Immich-Checksum`, which belongs to the unconverted file.
  - The app is not told a photo is safely backed up when its converted copy is in Immich's trash and only the marker went by.
  - A player or download manager that asks for one converted download in many pieces has it fetched and converted once, not once per piece.
  - An app that retried an upload while its conversion was still running held back other uploads for as long as that conversion took, with room in the temp folder it no longer used.
  - With an Immich address that has a path (`https://host/immich`), the app's live connection (websocket) went to the address without that path, and failed.
  - The check for room in the temp folder counted an upload still arriving as if its request were already on disk, and an upload without a size (chunked) as nothing, so several could together still fill the disk. An upload of unknown size is now taken on only when no other one is arriving.
  - Looking up a new upload's marker for the app's live connection could hold Immich's messages, its pings among them, for up to 30 seconds and make the app's connection drop; it now gives up after 5.
  - An asset going to the trash during sync was looked up again although its hidden entry had already been sent in the same stream.
  - `IO_TASKS_CONCURRENCY=1` is now also respected for converted downloads (it was raised to 2).
  - A converted download still being sent at shutdown left its temporary files behind.
- **Library mode**
  - A second copy was trashed against another user's conversion that this user could see through partner sharing or a shared album.
  - When Immich applied the move to the trash but its answer was lost, the verified copy was deleted, leaving the photo only in the trash.
  - Uploads made while library mode was not listening (stopped, disconnected, or a burst) are now checked for second copies.
  - After a failed write of the audit log, library mode stopped for good; it now goes on once the log can be written again.
  - An audit log entry written after a failed or interrupted write (a full disk, a crash) was joined to the broken line and lost; it now starts on a line of its own.
  - Second copies are found also when the default tasks file has no task for their type.
  - Two different API keys of the same user worked on the same assets at the same time, which could leave a photo only in the trash. The program now stops at startup with an error naming the user.
  - Albums, shared links, people and comments added to the original while it was being replaced (a conversion can take long) were lost with it: the original is now checked once more for them before it goes to the trash, and if anything was added it stays and is converted at a later pass.
  - An interrupted replacement could lose work: when the original had been restored from the trash, the verified copy was deleted with everything added to it since; when the person had trashed the original of a copy that was never verified, that copy was kept as if it were complete. The copy is now marked `verified` before the original goes to the trash, a verified copy is never deleted, and every case that cannot be completed as planned keeps both and tags the copy `immich-optimizer/review`.
  - Immich can apply the tags of the copied sidecar to the new asset late; a `convert` tag arriving that way made the copy be converted again. It is now removed from the copy as well.
  - A copy uploaded right before an interruption, before the audit log recorded its upload, was never found again and stayed in the timeline. The next start now looks for it among the recent uploads.

### Changed

- **A task whose result is not smaller hands the file to the next matching task**, instead of ending there with the original. The first task that makes the file smaller is used; if none does, the original is uploaded as before. A task with an empty command is always the last one tried. With `profile1`, a PNG that lossy JPEG XL makes bigger is now tried with caesium. Download tasks are not affected: the first that works is used, whatever its size.
- **New `gain_maps` setting in the tasks file** for HDR photos (a JPEG with a gain map, Ultra HDR), like `motion_photos`: `keep` (default: the task gets the file as it is), `drop` (only the ordinary photo, done with ExifTool) or `skip` (the task is not used for HDR photos, the next matching one is). The bundled profiles are unchanged. See "HDR photos" in docs/tasks.md.
- **HEIC, HEIF and AVIF motion photos are split by the optimizer**, like JPEG ones: `motion_photos` (`drop_video`, `drop_photo`) now applies to them before the task runs. The video is taken out of the file's `mpvd` box as it is; removing it from the photo is done with ExifTool (in the Docker image), and the result is checked to be a HEIF file without the video. Samsung's own index of the video (a `sefd` box) is turned into an empty `free` box, so nothing in the photo points at a video that is gone. Library mode also recognises these motion photos, so one is never converted as a whole.
- **`profile1` converts HEIC with two commands in `tasks.yaml`** (vips, then ExifTool for the EXIF) instead of the `heic2jxl.sh` script, which handled motion photos itself. With `motion_photos: keep` its HEIC task now converts the photo as it does for JPEG (the video does not survive the conversion, and the result does not claim one) instead of passing the file through; use `keep` on a passthrough task to keep a motion photo whole, as for JPEG. The profile's configuration fingerprint (`config.sha256` in the marker) changes.
- **Settings that cannot be read are refused at startup.** `IO_PROXY_CONVERT_UPLOADS=maybe` used to mean `false`, and `IO_TASKS_CONCURRENCY=abc` 0. `IO_LIBRARY_READY_TIMEOUT=0` is refused instead of meaning 10 minutes, and several API keys in watcher mode alone (which uploads for one user) are refused instead of all but the first being ignored. On/off settings take `true`/`false`, `yes`/`no`, `on`/`off` or `1`/`0`. An unknown `IO_LOG_LEVEL` is refused instead of meaning `info`.
- **A task that exits 0 without leaving exactly one file with content** counts as failed, so the next matching task is tried.
- On shutdown with several modes, proxy and watcher stop taking work at once, and all modes share one `IO_SHUTDOWN_TIMEOUT`.
- Library mode reads the whole library with Immich's current search instead of the one Immich will remove in version 4.
- Built with Go 1.26.11 (security fixes in the standard library).
- **Releases**: published only from `v*` tags, and only after the full CI (lint, tests, profile and end-to-end tests) passes on the tagged commit. GitHub Actions are pinned by commit, the image's base images by digest, and the downloaded tools (caesium-clt, ExifTool) are checked against their SHA-256.
- **Only the current Immich (3.x) is supported.** Uploads no longer send `deviceAssetId` and `deviceId`, which Immich 1.x and 2.x required and 3.x ignores.
