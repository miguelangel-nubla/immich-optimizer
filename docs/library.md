# Library mode

Proxy and watcher mode only handle new uploads. Library mode (`IO_MODE=library`) goes through the assets **already in Immich** and replaces each one with its converted copy. It needs an API key and works for that key's user only: run it once per user.

```yaml
services:
  immich-optimizer-library:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_MODE=library
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_IMMICH_API_KEY=your-api-key
      # - IO_LIBRARY_DRY_RUN=false      # the default is a dry run that changes nothing
    volumes:
      - ./undone:/undone                # the log is written here
    restart: "no"                       # it makes one pass and exits
```

Everything it changes can be undone from Immich's trash, and nothing is changed until you say so: **it starts as a dry run.** Without `IO_LIBRARY_DRY_RUN=false` it changes nothing and writes a [report](#the-report) of what it would do.

## The dry run

A dry run goes through the whole library and decides, for every asset, whether it would be converted and, if not, why. It then **downloads and converts a small random sample** of the candidates (20 by default, [`IO_LIBRARY_DRY_RUN_SAMPLE`](configuration.md#library-mode)) to find out what a real run would do: how much smaller the files get, whether any fail, and what the results look like. The converted sample is thrown away. **Nothing is uploaded, marked or moved to the trash.**

Without the sample (`IO_LIBRARY_DRY_RUN_SAMPLE=0`) a dry run downloads nothing and can only say how many assets are candidates.

## The report

Every run, dry or real, writes `library-report.html` to the undone folder ([`IO_LIBRARY_REPORT_FILE`](configuration.md#library-mode)). It is a single file with no outside dependencies: copy it to your computer and open it in a browser.

- **Totals:** assets looked at, would be converted (or replaced), kept, skipped and failed.
- **Estimated saving** (dry run): the sample's result applied to all candidates, each file type by its own figure. With a small sample it is a rough number.
- **By outcome and by file type:** how many assets and how many bytes are skipped for which reason. Select a row to filter the list.
- **Converted sample** (dry run): each sampled file with a preview of the original next to a preview of the conversion, both sizes and the task used.
- **Every asset** in a table you can search, filter, sort and page through, with the reason it was skipped or the error it failed with.
- **Links:** each file name opens the asset in Immich. Set [`IO_IMMICH_PUBLIC_URL`](configuration.md#immich) to the address you use in your browser, or type it into the box on the page. For a replaced asset the link goes to the new asset; its original is in Immich's trash.

The report is rewritten by each run and shows that run only. The [audit log](audit-log.md) is the permanent record.

## What happens to each asset

Immich has no "replace this file", so the optimizer does it in a fixed order that never leaves you without your photo:

1. Download the original and check it against Immich's checksum.
2. Convert it with your tasks, then check that the result **looks like a valid photo or video**: the file is not empty, it decodes completely, and it is not a single flat colour (unless the original was). For a video: it has a picture track, a length, and a frame from the middle decodes.
3. Upload the converted file as a new asset.
4. Carry everything over: albums, shared links, stack position, favorite, archive state, description, rating, date and location edits, tags, people (named faces) and the mobile app's own metadata.
5. Compare the new asset with the original, field by field.
6. **Only if everything matches**, move the original to Immich's **trash**.

If anything fails or differs before step 6, the new asset is deleted again and the original is left exactly as it was. Because the original goes to the trash, you can restore it from Immich for as long as your trash keeps it (30 days by default). The space is freed when the trash is emptied.

## What is carried over, and what is not

The converted copy is a new asset in Immich. This is everything the original had, and what happens to it:

| | What | How |
|---|---|---|
| ✅ | Albums (including shared ones) | copied by Immich |
| ✅ | Individual shared links | copied by Immich, completed by the optimizer |
| ✅ | Stack membership, and being the stack's cover | copied by Immich; cover set by the optimizer |
| ✅ | Favorite, archived state | set on upload |
| ✅ | Description, rating | copied by Immich with the sidecar; set by the optimizer if they still differ |
| ✅ | Date and location you edited in Immich | same |
| ✅ | Tags | same |
| ✅ | People (faces assigned to a person) | Immich detects them again in the new file; any still missing are recreated by the optimizer with the same face box |
| ✅ | File dates, date taken, time zone, GPS, camera and lens, dimensions, orientation | read by Immich from the converted file, then compared |
| ✅ | Edits made in Immich (crop, rotate, mirror) | copied by the optimizer. Only possible when the conversion keeps the picture's size, see below |
| ✅ | The video of a live photo or motion photo | the copy is paired with the same video, which is left as it is |
| ✅ | Asset metadata written by apps (for example the mobile app's iCloud id) | copied by the optimizer |
| ✅ | Owner, type, file name (with the new extension) | by upload |
| ⚠️ | Faces not assigned to anyone | not copied: Immich detects them again |
| ⚠️ | A face you removed by hand, or hid | may come back when Immich detects faces again |
| ⚠️ | Thumbnails, search index, face data | generated again by Immich for the new file |
| ❌ | The asset's id | the copy has a new one: links and bookmarks to the old asset stop working |
| ❌ | The upload date ("recently added") | becomes the day of the conversion |
| ❌ | Membership in a memory | not carried over |
| ❌ | Being an album's cover or a person's featured photo | Immich picks another one |

Every ✅ row is checked in step 5: if the copy differs from the original in any of them, it is deleted and the original stays. A few allowances are made on purpose:

- The file name is compared without its extension.
- The copy may belong to more people than the original, never fewer.
- **Dimensions and orientation are not compared**, because a task may resize, crop or rotate. A video's length may differ by half a second or 2%.

The validity check in step 2 does **not** compare the picture with the original either: a task is free to crop, resize, watermark or re-encode. It only rejects results that are broken. Library mode converts with whatever your tasks file says, and treats every task the same way.

## What is skipped

These assets are left exactly as they are. The reason for each is written to the debug log; assets marked 📝 also get an audit log entry.

| Asset | Why |
|---|---|
| Its task is a passthrough, or no task handles its type | there is nothing to convert |
| It does not get smaller 📝 | the original is kept and marked, so later runs do not download it again |
| A motion photo whose video Immich did not extract 📝 | converting the photo would lose the video |
| A motion photo with `motion_photos: keep` or `drop_photo` 📝 | the result would hide or drop a part of it. Use `drop_video`, as the bundled profiles do |
| Has comments or likes in a shared album | they belong to the asset and cannot be moved to the copy |
| In the locked folder | not visible to an API key |
| In an external library | Immich does not own those files |
| In the trash, hidden, or offline | not part of the visible library |
| Belongs to another user or a partner | each user runs library mode with their own key |
| Already converted, or already looked at | recognised by its [marker](marker.md) |
| Newer than `IO_LIBRARY_TAKEN_BEFORE` | your own limit |
| Immich has not finished reading it yet | it is picked up by a later run |
| It changed while it was being replaced | you edited it, or Immich was still analysing it. The copy is removed and a later run converts it |

## Live photos, motion photos and edits

- **Live photos (Apple) and motion photos (Android)** are a photo paired with a short video. In Immich the video is an asset of its own, hidden behind the photo. Library mode converts the photo and pairs the copy with that same video. The video itself is not converted.
- **Edits made in Immich** are stored as positions on the original picture. They are copied to the new asset when it has the same pixel size. If your task resizes or crops, an edited asset fails the comparison and is left as it is; unedited assets are converted normally.

## Things to know before running it

- **The official mobile app will upload the originals again** for photos that are still on a phone with backup enabled, because the server no longer has a file with the checksum the app looks for. Use `IO_LIBRARY_TAKEN_BEFORE` to limit the run to photos that are no longer on your phone. See [Proxy mode limitations](proxy.md#limitations).
- **Android: turn off "Sync remote deletions" in the Immich app** (Settings → Advanced, marked experimental, off by default) before a real run. With it on, the app deletes a photo from the phone when the same photo is moved to the trash on the server, which is exactly what library mode does with each original.
- **Space is freed when Immich empties its trash**, not at once. Until then the original also counts against a user's storage quota.
- It waits for Immich to finish reading each new file, so a large library takes a long time. By default it works on one asset at a time; [`IO_LIBRARY_CONCURRENCY`](configuration.md#library-mode) raises that, at the cost of more load on Immich. A run can be stopped at any point and started again: it continues where it left off, and finishes or undoes whatever was in progress.
- Start with a small batch (`IO_LIBRARY_LIMIT=20`) and look at the result in Immich before letting it run through everything.

Every step is written to the [audit log](audit-log.md).

## Settings

See [Library mode settings](configuration.md#library-mode). The API key and Immich address are in [Immich settings](configuration.md#immich).
