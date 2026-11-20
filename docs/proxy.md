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

- **What is converted:** multipart uploads to [`IO_PROXY_FILTER_PATH`](configuration.md#proxy-mode). The file is handled as described in [Tasks](tasks.md#how-a-file-is-handled).
- **Errors:** a file type no task handles answers `415`; a failed conversion answers `502`. Nothing is uploaded in either case and the original is kept in the [undone folder](undone.md). If Immich itself cannot be reached or refuses the upload, the client gets the error and nothing is kept: it still has the file and retries.
- **Jobs survive client disconnects.** Once the upload has been received, converting it and sending it to Immich no longer depend on the client connection. If the phone disconnects or gives up waiting, the job continues and the asset still reaches Immich.
- **No double uploads.** If the same user uploads the identical file again while it is being processed, or within 30 minutes afterwards, the retry gets the same result instead of a second conversion. Transient answers (`5xx`, `408`, `413`, `425`, `429`) are not replayed.
- **Browsers** are kept waiting with periodic redirects while a long conversion runs.
- Each converted asset gets a [provenance marker](marker.md), and each upload a line in the [audit log](audit-log.md).

## Limitations

Read this before putting the proxy in front of the official Immich mobile app.

- **The mobile app re-uploads converted files.** The app decides that a photo is backed up by comparing the checksum of the file on the phone with the checksums on the server. A converted file has a different checksum, so the app never sees its photo as backed up and uploads it again on every backup run. Each upload is converted again and Immich answers "duplicate", so no second copy is stored when the conversion gives the same bytes every time, but the phone keeps sending the data and the app keeps showing the photo as not backed up.
- **Lossy and video tasks can create real duplicates** if the encoder does not produce identical bytes for the same input.
- **This cannot be fixed inside the proxy alone**: Immich offers no way to store a different checksum for an asset.

What works well today:

- **Proxy mode for the web interface, `immich-go` and the Immich CLI.** These upload a file once.
- **Watcher mode for phones**: sync the phone's camera folder to the watch folder with a one-way sync tool (Syncthing send-only, FolderSync upload-only), and deselect every album in the app's backup settings. The app is then a viewer only.

Every converted asset carries a [provenance marker](marker.md) with the checksum of its original, so a later fix can recognise assets that are already uploaded.

## Experimental: making the mobile app see converted photos as backed up

`IO_PROXY_EXPERIMENTAL_APP_SYNC=true` is an attempt to remove the limitation above. It is off by default and has **not been confirmed on a real phone yet**.

**How it works.** The app keeps its own list of what is on the server, filled from Immich's sync, and treats a photo as backed up when that list has an entry with the photo's checksum. Every converted asset carries a [marker](marker.md) with the checksum of its original, and the marker is part of the same sync. When a marker passes through the proxy, the proxy adds one hidden entry with the original's checksum to the answer. The app finds its match and stops uploading the photo.

- Nothing is stored: the hidden entry is built from the marker as it goes by.
- The real asset's entry is not changed.
- The hidden entry is never shown, and it disappears from the phone when the converted asset is deleted.
- Requests the app makes about a hidden entry are answered with the real asset.

**What to know before trying it**

- **The app must sync through the proxy.** A phone only gets a hidden entry when the marker reaches it through the proxy. Assets it had already synced before the setting was turned on do not get one until the app does a full resync (log out and in again).
- **"Free up space" will trust it.** The app will consider those photos safely backed up and offer to delete them from the phone, although the server holds the converted file, not the original.
- It depends on the format of Immich's sync, which can change with any Immich release. If the proxy does not recognise a line it passes it through unchanged, so the worst case is that the app goes back to uploading again.
- Turning it off again does not remove hidden entries already on a phone; a full resync does.
