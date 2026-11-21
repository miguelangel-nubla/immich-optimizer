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

## Converting only on demand

With `IO_PROXY_CONVERT_UPLOADS=false` the proxy does not convert uploads: they reach Immich exactly as they were sent. Use it when you want to choose what is converted, with tags in Immich and [library mode](library.md):

```yaml
    environment:
      - IO_MODE=proxy,library
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_IMMICH_API_KEY=key-of-alice,key-of-bob
      - IO_PROXY_CONVERT_UPLOADS=false
```

The proxy then still does the two things only it can do:

- **The mobile app** keeps counting photos that library mode converted as backed up, and does not upload them again ([below](#the-mobile-app)).
- **Downloads** of converted photos are converted back, for example JPEG XL to JPEG ([Tasks: downloads](tasks.md#downloads)).

To convert every new upload anyway, but in the background instead of during the upload, let one of Immich's workflows tag new uploads `immich-optimizer/convert` ([Library mode](library.md#choosing-what-to-convert)).

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

The official app decides that a photo is backed up by comparing the checksum of the file on the phone with the checksums Immich reports. A converted file has a different checksum, so left to itself the app would not recognise its photos once they are converted, and would upload them again and again.

The proxy takes care of this: it tells the app the checksums of the originals. The app counts converted photos as backed up and uploads each one once. There is nothing to set.

- **The app must reach Immich through the proxy** for everything, not only for uploads. A phone that talks to Immich directly part of the time does not learn the originals' checksums there.
- **A phone that already knew your photos catches up by itself** when it starts going through the proxy. At worst it uploads each converted photo one more time: nothing is stored twice, and after that it counts them as backed up.
- **"Free up space" trusts it.** The app considers those photos safely backed up and offers to delete them from the phone, although the server holds the converted file, not the original. It does not offer photos whose converted copy is in Immich's trash.
- **It follows what you do in Immich.** Trash a converted photo, restore it or delete it for good, and the app treats the photo on the phone as it would with any other photo.
- **It covers [library mode](library.md#phones) too**: photos converted there are not uploaded again either.
- **Lossy and video tasks** should give the same result for the same file every time. The app uploads a photo once, but if it ever does so again (after a reinstall, say) and the conversion comes out different, Immich stores a second copy.

**What it is tested with.** The real Immich apps (v3.4.2), in the project's own tests: Android in an emulator, iOS in Xcode's simulator. [Details.](app-sync.md#what-it-is-tested-with)

**If Immich changes** the way the app and the server talk, the proxy passes on untouched whatever it no longer understands. The worst case is that the app goes back to uploading again; nothing in Immich is affected.

How it works is described in [How app sync works](app-sync.md).

If you would rather keep the app out of uploading altogether, use [watcher mode for phones](watcher.md#using-it-for-phone-backup): a sync tool brings the photos, and the app is a viewer only.
