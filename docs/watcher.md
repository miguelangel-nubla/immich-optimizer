# Watcher mode

The optimizer watches a folder. Every file that appears in it is converted, uploaded to Immich with an API key, and then removed from the folder.

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_MODE=watcher
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_IMMICH_API_KEY=your-api-key
    volumes:
      - /path/to/watch:/watch
      - ./undone:/undone
    restart: unless-stopped
```

**Before the first start on Linux**, create the `undone` folder and give it, and the watch folder, to the user the optimizer runs as: `mkdir -p undone && sudo chown 1001 undone /path/to/watch`. The optimizer deletes what it has uploaded from the watch folder, so it must be able to write there too. More in [Folders and ports](docker.md#folders-and-ports).

Uploads go to the API key's user: run one container per user. Settings: [Watcher mode](configuration.md#watcher-mode).

## How it behaves

- Files are handled as described in [Tasks](tasks.md#how-a-file-is-handled). Files already in the folder at startup are processed too, and subfolders are watched.
- **A file is taken once it is complete**: when the program writing it closes it, or renames it into place. A file that is still open is never taken, however long its writer pauses. A file found already there (at startup, or in a folder copied or moved in), or one published with a hard link (which sends no close event under its own name), is taken once it has not been written to for 10 seconds. A file that is written to while it is being converted is not uploaded: it is kept and handled again.
- **Dates**: the asset's file dates sent to Immich are those of the original file in the watch folder, not of the converted output. Photos without a date in their metadata land on the right day as long as the sync tool preserved the file's modification time.
- **Ignored files**: hidden files and folders (names starting with a dot, which covers the in-progress files of rsync and Syncthing, `.DS_Store` and macOS `._*` files), `Thumbs.db`, `desktop.ini` and common temporary extensions (`.tmp`, `.part`, `.crdownload`...) are left where they are. They are neither uploaded nor moved to undone.
- **After a successful upload the source file is deleted** from the watch folder. Use a one-way sync into it.
- **A file that cannot be converted or uploaded** is moved to the [undone folder](undone.md). If that fails too (for example the disk is full), it stays in the watch folder.
- If more files arrive at once than the system can report, the folder is scanned again, so nothing is missed.
- The watch folder must be on a local file system: changes made on a network share by another machine are not noticed.
- Each converted asset gets a [provenance marker](marker.md), and each upload a line in the [audit log](audit-log.md).

## Importing a folder of files

Watcher mode is a good fit when you have photos and videos as plain files, in a folder structure of your own (an old archive, a camera card, a disk you are clearing), and want them **converted and uploaded in one step**. Copy them into the watch folder; each one is converted and uploaded, and the folder empties as it goes. Nothing else is needed: no [Immich CLI](https://docs.immich.app/features/bulk-upload) (`immich upload`) and no [immich-go](https://github.com/simulot/immich-go) (`immich-go upload from-folder`), the tools usually used to upload folders to Immich.

- **Copy, do not move, your only copy.** Each file is deleted from the watch folder once Immich has it. Keep the originals elsewhere until you have checked the result in Immich.
- **Subfolders are fine**, at any depth. The folder structure is not turned into albums: every file simply lands in the timeline, on its own date.
- **A file that cannot be converted is not uploaded.** It goes to the [undone folder](undone.md), with the reason, so a large import never ends up with broken or unconverted files in it.
- **Large imports** run at the pace of the conversions ([`IO_TASKS_CONCURRENCY`](configuration.md#tasks)); copy in batches if the disk the watch folder is on is small.

If you need what those tools do beyond uploading (an album per folder, importing a Google Photos takeout), use them anyway, pointed at the [proxy](proxy.md) instead of at Immich: what they upload is converted on the way in. Both are tested this way.

## Using it for phone backup

Sync the phone's camera folder into the watch folder with a one-way sync tool (Syncthing "send only", FolderSync "to remote folder"), and deselect every album in the Immich app's backup settings. The app is then a viewer only, and its checksum-based backup, described in [The mobile app](proxy.md#the-mobile-app), plays no part.
