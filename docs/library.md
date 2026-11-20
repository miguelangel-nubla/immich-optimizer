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
    restart: "no"                       # it makes one pass and exits (or runs on a schedule, see below)
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
| Does not match your `library_filter` | see [Choosing what to convert](#choosing-what-to-convert) |
| A second copy of a converted original that has something of its own 📝 | see [Second copies](#second-copies-of-converted-originals) |
| Immich has not finished reading it yet | it is picked up by a later run |
| It changed while it was being replaced | you edited it, or Immich was still analysing it. The copy is removed and a later run converts it |

## Choosing what to convert

By default library mode converts everything your tasks can handle. To limit it, add a `library_filter` block to your [tasks file](tasks.md). It is a filter of **Immich's own asset search**: the optimizer passes it to Immich unchanged, and Immich says which assets match.

```yaml
library_filter:
  takenAt:
    lt: 2024-01-01T00:00:00Z        # taken before 2024
  fileSizeInBytes:
    gt: 2000000                     # larger than 2 MB
  albumIds:
    none:                           # not in this album
      - 3fa85f64-5717-4562-b3fc-2c963f66afa6

tasks:
  # ...
```

- **Several conditions must all match.** For "either of these", list them under `or:`.
- **The fields and operators are Immich's**, and their names are case sensitive. Among them: `takenAt`, `createdAt`, `fileSizeInBytes`, `type`, `originalFileName`, `make`, `model`, `city`, `country`, `isFavorite`, `rating`, `albumIds`, `personIds`, `tagIds`, `hasAlbums`. The operators depend on the field: `eq`/`ne`, `lt`/`lte`/`gt`/`gte`, `in`/`notIn`, `like`/`startsWith`/`endsWith`, and `any`/`all`/`none` for lists of ids. The full list is in [Immich's API reference](https://api.immich.app/), under the `filter` field of the asset search (`POST /search/metadata`).
- **A filter Immich does not accept stops the run** before anything is changed, with Immich's own message.
- **Check it with a dry run.** The [report](#the-report) lists what would be converted; everything else shows as skipped with "it does not match library_filter".
- **To try library mode on a few photos first**, put them in an album and filter on `albumIds`.
- It needs Immich v3.2 or newer. Dates are absolute; there is no "older than 90 days".
- The filter only says what to convert. [Second copies](#second-copies-of-converted-originals) and replacements an earlier run left unfinished are settled whatever it says.

## Second copies of converted originals

An original can come back after it was converted: the mobile app uploads a photo again when the server no longer has a file with its checksum, which is the case once the original has left Immich's trash (see [Phones](#phones)).

Library mode recognises such a file without converting it again. Every converted asset carries a [marker](marker.md) with the checksum of its original, and each run reads them all. An unconverted asset whose file has that checksum is a second copy of the same photo.

- **It is moved to the trash**, and the audit log gets a `second_copy_trashed` entry that names both assets. The converted asset is not touched.
- **Nothing is remembered between runs.** The lookup is built from the markers on every run.
- **It is left alone when it has something the converted asset lacks**: it is a favorite, has a description, a rating, tags or edits, or is in an album, stack or shared link the converted asset is not in. The report says which, so you can merge the two by hand.
- **A dry run** only reports it (`would_trash_second_copy`).

## Phones

The official mobile app decides what to back up by checksum: a photo counts as backed up while the server has a file with the same checksum, in the timeline or in the trash. A converted file has another checksum.

**With the proxy's app sync, a phone is not affected.** When the phone reaches Immich through [proxy mode](proxy.md) with the [experimental app sync](proxy.md#experimental-making-the-mobile-app-see-converted-photos-as-backed-up) on, it learns the original's checksum from each conversion's [marker](marker.md), whether the proxy or library mode made the conversion. Photos that are still on the phone are converted like any other and are not uploaded again. This is tested with the real Android app in an emulator: library mode converts photos the phone has, the trash is emptied, and the app uploads nothing.

**Without it**, a photo that is still on a phone with backup on comes back:

1. Library mode converts the photo and moves the original to the trash. The app still finds the original's checksum there and stays quiet.
2. Immich empties the trash (30 days by default). The checksum is gone, and the app uploads the photo again.
3. The next library run finds the [second copy](#second-copies-of-converted-originals) and moves it to the trash, which starts at 1 again.

Nothing is lost, but such a photo saves no space: its original is in the trash most of the time.

## Running it on a schedule

`IO_LIBRARY_INTERVAL` keeps library mode running: one run, then another that long after it ends, such as `24h`. Use it with `restart: unless-stopped` instead of `restart: "no"`.

```yaml
    environment:
      - IO_MODE=library
      - IO_LIBRARY_DRY_RUN=false
      - IO_LIBRARY_INTERVAL=24h
    restart: unless-stopped
```

Durations are written as hours (`h`), minutes (`m`) and seconds (`s`). A run that fails does not end the schedule: the next one starts after the interval and finishes whatever was left.

## Live photos, motion photos and edits

- **Live photos (Apple) and motion photos (Android)** are a photo paired with a short video. In Immich the video is an asset of its own, hidden behind the photo. Library mode converts the photo and pairs the copy with that same video. The video itself is not converted.
- **Edits made in Immich** are stored as positions on the original picture. They are copied to the new asset when it has the same pixel size. If your task resizes or crops, an edited asset fails the comparison and is left as it is; unedited assets are converted normally.

## Things to know before running it

- **A phone that still has the photos uploads them again** once the originals have left Immich's trash, unless it reaches Immich through the proxy with app sync on. See [Phones](#phones).
- **Android: turn off "Sync remote deletions" in the Immich app** (Settings → Advanced, marked experimental, off by default) before a real run. With it on, the app deletes a photo from the phone when the same photo is moved to the trash on the server, which is exactly what library mode does with each original.
- **Space is freed when Immich empties its trash**, not at once. Until then the original also counts against a user's storage quota.
- It waits for Immich to finish reading each new file, so a large library takes a long time. By default it works on one asset at a time; [`IO_LIBRARY_CONCURRENCY`](configuration.md#library-mode) raises that, at the cost of more load on Immich. A run can be stopped at any point and started again: it continues where it left off, and finishes or undoes whatever was in progress.

Every step is written to the [audit log](audit-log.md).

## Settings

See [Library mode settings](configuration.md#library-mode). The API key and Immich address are in [Immich settings](configuration.md#immich).
