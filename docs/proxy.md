# Proxy mode

The optimizer sits between your Immich clients and Immich. Uploads are converted on the way in; every other request is passed straight through.

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_MODE=proxy
      - IO_IMMICH_URL=http://immich-server:2283
    ports:
      - "2283:2283"
    volumes:
      - ./undone:/undone
    stop_grace_period: 2m       # lets running conversions finish, see Docker notes
    restart: unless-stopped

  immich-server:
    # ...your existing Immich service, without its "ports:" section
```

Point your clients at the optimizer instead of Immich. If Immich sits behind your own reverse proxy (nginx, Traefik, Caddy), point that proxy at the optimizer.

- **No API key is needed.** Each client's own credentials are forwarded, so it works for every user.
- Settings: [Proxy mode](configuration.md#proxy-mode).

## How it behaves

- **What is converted:** multipart uploads to Immich's upload endpoint (`/api/assets`). The file is handled as described in [Tasks](tasks.md#how-a-file-is-handled).
- **Errors:** a file type no task handles answers `415`; a failed conversion answers `502`. Nothing is uploaded in either case and the original is kept in the [undone folder](undone.md). If Immich itself cannot be reached or refuses the upload, the client gets the error and nothing is kept: it still has the file and retries. `503` means "try again later": the proxy is shutting down, has no room in its temp folder, or cannot write its audit log.
- **What the mobile app does with an error** (tested with the Android app v3.4.2 in an emulator): it tries each failed file once every time it looks for things to back up, does not retry in between, and carries on with the other files. Once the cause is gone, the file is backed up on the next round.
- **A dropped connection does not lose the upload.** Once the proxy has received a file, it finishes converting it and stores it in Immich, even if the phone goes offline or gives up waiting in the meantime.
- **A repeated upload is not stored twice.** If the same person sends the identical file again while it is being converted, or within 30 minutes afterwards, they get the answer to the first upload instead of a second conversion. Two exceptions, where the file is handled afresh: the first upload failed for a temporary reason (`5xx`, `408`, `413`, `425`, `429`), or the photo it created has since been deleted from Immich.
- **Uploads wait when the temp folder is filling up.** Every upload that is waiting or being converted has files in the container's temp folder (`/tmp`), up to three times its size. Before reading a new upload, the proxy checks that there is room for it next to the ones in progress. If not, the client is held for up to 45 seconds and then answered `503` with `Retry-After`, so a phone that sends faster than your tasks convert slows down instead of making the conversions under way fail for lack of space. There is nothing to set: the limit is the free space itself.
- **A long conversion does not time out in the browser.** The proxy keeps the upload alive until the file is stored, however long the conversion takes.
- **The file's name** is taken from the upload's `filename` field when it has one, as Immich does; the mobile app's background upload sends the real name there.
- **Downloads can be converted back.** With a [`downloads` list](tasks.md#downloads) in the tasks file, an original stored as JPEG XL is handed out as JPEG when someone downloads it.
- Each converted asset gets a [provenance marker](marker.md), and each upload a line in the [audit log](audit-log.md).

## The mobile app

The official app decides that a photo is backed up by comparing the checksum of the file on the phone with the checksums Immich reports. A converted file has a different checksum. What follows from that depends on one setting.

**With [`IO_PROXY_EXPERIMENTAL_APP_SYNC=true`](#experimental-making-the-mobile-app-see-converted-photos-as-backed-up)** the app sees converted photos as backed up and uploads each one once. This is tested with the real Android app in an emulator; it has not been tried on iOS. It is off by default because it depends on the format of Immich's sync, which can change with any release.

**Without it** the app never sees a converted photo as backed up:

- It uploads every converted photo again each time it looks for things to back up, and shows it as not backed up.
- No second copy is stored. Within 30 minutes the proxy answers the retry from memory; after that it converts again and Immich answers "duplicate", as long as the conversion gives the same bytes every time.
- **Lossy and video tasks can create real duplicates** if the encoder does not produce identical bytes for the same input.

Everything else works either way: the web interface, `immich-go`, the Immich CLI and uploads through a shared link upload a file once.

If you would rather keep the app out of uploading altogether, use [watcher mode for phones](watcher.md#using-it-for-phone-backup): a sync tool brings the photos, and the app is a viewer only.

## Experimental: making the mobile app see converted photos as backed up

`IO_PROXY_EXPERIMENTAL_APP_SYNC=true` makes the proxy tell the app the checksums of the originals. It is off by default. It is tested with the real Android app in an emulator (Immich app v3.4.2): without it the app sends every converted photo again each time it looks for things to back up, with it the app counts them as backed up and uploads each one once. It has not been tried on iOS.

**How it works.** The app keeps its own list of what is on the server, filled from Immich's sync, and treats a photo as backed up when that list has an entry with the photo's checksum. Every converted asset carries a [marker](marker.md) with the checksum of its original, and the marker is part of the same sync. When a marker passes through the proxy, the proxy adds one hidden entry with the original's checksum to the answer. The app finds its match and stops uploading the photo.

- The proxy keeps no list of its own. It works out each hidden entry from the marker Immich sends, every time.
- The real asset's entry is not changed.
- The hidden entry is never shown.
- **It arrives at once.** Immich tells the app about a finished upload over a websocket, and the proxy adds the hidden entry there too, so the app's backup page counts the photo as backed up right away.
- **It follows its asset.** It is in the trash while the converted asset is, and it is deleted from the phone when the converted asset is deleted for good or loses its marker. The app then treats the photo as it treats any photo whose server copy was trashed or deleted.
- If the app ever asks for a hidden entry, for a thumbnail say, it gets the converted photo.
- **It covers [library mode](library.md#phones) too.** Assets that library mode converted carry the same marker, so the phone does not upload their originals again after they leave the trash.

**What to know before trying it**

- **The app must reach Immich through the proxy**, for everything: uploads, sync and the websocket.
- **Turning it on later works.** A phone that already knows your photos catches up by itself. At worst it uploads each converted photo one more time: nothing is stored twice, and after that it counts them as backed up.
- **"Free up space" will trust it.** The app considers those photos safely backed up and offers to delete them from the phone, although the server holds the converted file, not the original. It does not offer photos whose converted asset is in the trash.
- **It costs a little extra work.** For each photo the app is told about, the proxy asks Immich one more question, to learn whether the photo has a marker. In a sync with thousands of changes it asks for the first 200 only; the others get their hidden entry from their marker, as usual. It also reads the live connection between the app and Immich, which it therefore asks Immich not to compress.
- It depends on the format of Immich's sync and websocket messages, which can change with any Immich release. What the proxy does not recognise it passes through unchanged, so the worst case is that the app goes back to uploading again.
- Turning it off again does not remove hidden entries already on a phone; a full resync does (log out and in again).

