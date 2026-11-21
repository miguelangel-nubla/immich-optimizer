# Watcher mode

The optimizer watches a folder. Every file that appears in it is converted, uploaded to Immich with an API key, and then removed from the folder.

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_IMMICH_API_KEY=your-api-key
    volumes:
      - /path/to/watch:/watch
      - ./undone:/undone
    restart: unless-stopped
```

Watcher is the default mode, so `IO_MODE` is not needed. Uploads go to the API key's user: run one container per user. Settings: [Watcher mode](configuration.md#watcher-mode).

## How it behaves

- Files are handled as described in [Tasks](tasks.md#how-a-file-is-handled). Files already in the folder at startup are processed too, and subfolders are watched.
- **Dates**: the asset's file dates sent to Immich are those of the original file in the watch folder, not of the converted output. Photos without a date in their metadata land on the right day as long as the sync tool preserved the file's modification time.
- **Ignored files**: hidden files and folders (names starting with a dot, which covers the in-progress files of rsync and Syncthing, `.DS_Store` and macOS `._*` files), `Thumbs.db`, `desktop.ini` and common temporary extensions (`.tmp`, `.part`, `.crdownload`...) are left where they are. They are neither uploaded nor moved to undone.
- **After a successful upload the source file is deleted** from the watch folder. Use a one-way sync into it.
- **A file that cannot be converted or uploaded** is moved to the [undone folder](undone.md). If that fails too (for example the disk is full), it stays in the watch folder.
- If more files arrive at once than the system can report, the folder is scanned again, so nothing is missed.
- The watch folder must be on a local file system: changes made on a network share by another machine are not noticed.
- Each converted asset gets a [provenance marker](marker.md), and each upload a line in the [audit log](audit-log.md).

## Using it for phone backup

Sync the phone's camera folder into the watch folder with a one-way sync tool (Syncthing "send only", FolderSync "to remote folder"), and deselect every album in the Immich app's backup settings. The app is then a viewer only, and its checksum-based backup, described in [The mobile app](proxy.md#the-mobile-app), plays no part.
