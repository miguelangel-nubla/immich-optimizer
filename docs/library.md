# Library mode

Proxy and watcher mode only handle new uploads. Library mode (`IO_MODE=library`) converts the assets **already in Immich**, replacing each one with its converted copy. **You choose which ones in Immich itself, with a tag:** it converts only what is tagged `immich-optimizer/convert`. See [Choosing what to convert](#choosing-what-to-convert). It needs an API key for each user whose photos it should convert.

> **Using the Immich mobile app to back up?** Run library mode together with [proxy mode](proxy.md), and have the phones reach Immich through the proxy. The proxy tells the app that a converted photo is still the photo it has, so the app does not upload the original again. Without it, every converted photo that is still on a phone is uploaded again once its original has left Immich's trash. See [Phones](#phones).

The recommended setup, proxy and library mode in one container (see the [quick start](quickstart.md) for the proxy part):

```yaml
services:
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_MODE=proxy,library
      - IO_IMMICH_URL=http://immich-server:2283
      - IO_IMMICH_API_KEY=key-of-alice,key-of-bob   # one key per user, all permissions
    ports:
      - '2283:2283'
    volumes:
      - ./undone:/undone                # the audit log is written here
    restart: unless-stopped
```

It keeps running and waits. **Nothing happens until you tag something** in Immich: tag a few photos `immich-optimizer/convert` and they are converted within seconds. Everything it replaces can be undone from Immich's trash.

**The API keys.** Immich lets a key act on its owner's photos only, and has no key that works for every user. So each user whose photos should be converted creates a key once, in Immich under Account Settings → API Keys, with **all permissions**, and gives it to you.

- **One key per user**, all in one container: separate them with commas in `IO_IMMICH_API_KEY`, or put one per line in the file named by `IO_IMMICH_API_KEY_FILE` (blank lines and lines starting with `#` are ignored, so you can note whose each key is).
- Library mode then works for each user separately: each one's tags act on that user's photos only. In the log, each user's lines start with `[user <id>]`, the user's id in Immich, which stays the same when a key is replaced. At start each one logs whose it is: `Library: working for Alice <alice@example.com> (user <id>)`. Until Immich has said whose a key is (while Immich cannot be reached, for example), its lines say `[key 2 of 3]`.
- All users share the conversions running at the same time ([`IO_TASKS_CONCURRENCY`](configuration.md#tasks)) and one [audit log](audit-log.md), in which each library entry names its user.
- Watcher mode uploads with the first key.

**To try it**, tag three or four photos and look at the result in Immich. Each converted photo is a new asset, and its original is in the trash, where you can compare the two or restore the original.

## How it knows what to do

Library mode keeps a live connection to Immich, the same one the web and mobile apps use. Immich tells it within a second or two when an asset is tagged, and it acts on it. Every 15 minutes it also looks for tagged assets by itself, in case a message was missed.

When it starts, it settles anything an earlier run left unfinished (a replacement interrupted by a restart, for example) and then acts on whatever is tagged. To know what may be unfinished, and which originals were converted into what, it reads its [audit log](audit-log.md) instead of every asset in Immich. The log is only used to know where to look: before acting, the optimizer always checks the asset's [marker](marker.md) in Immich. If the log is missing or empty, it reads the whole library once instead, which takes a while on a large library.

## What happens to each asset

Immich has no "replace this file", so the optimizer does it in a fixed order that never leaves you without your photo:

1. Download the original and check it against Immich's checksum.
2. Convert it with your tasks, then check that the result **looks like a valid photo or video**: the file is not empty, it decodes completely, and it is not a single flat colour (unless the original was). For a video: it has a picture track, a length, and a frame from the middle decodes.
3. Upload the converted file as a new asset.
4. Carry everything over: albums, shared links, memories, stack position, favorite, archive state, description, rating, date and location edits, tags, people (named faces) and the mobile app's own metadata.
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
| ✅ | Membership in memories ("on this day") | added by the optimizer |
| ✅ | Owner, type, file name (with the new extension) | by upload |
| ⚠️ | Faces not assigned to anyone | not copied: Immich detects them again |
| ⚠️ | A face you removed by hand, or hid | may come back when Immich detects faces again |
| ⚠️ | Thumbnails, search index, face data | generated again by Immich for the new file |
| ❌ | The asset's id | the copy has a new one: links and bookmarks to the old asset stop working |
| ❌ | The upload date ("recently added") | becomes the day of the conversion. Immich sets it itself and its API has no way to change it |
| ❌ | Being an album's cover or a person's featured photo | Immich picks another one |

Every ✅ row is checked in step 5: if the copy differs from the original in any of them, it is deleted and the original stays. A few allowances are made on purpose:

- The file name is compared without its extension.
- The copy may belong to more people than the original, never fewer.
- Memories are not read back: adding the copy to a memory is confirmed by Immich's answer, and a refusal undoes the replacement like any other difference.
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
| Not tagged `immich-optimizer/convert` | see [Choosing what to convert](#choosing-what-to-convert) |
| Already converted, already looked at, or tagged `keep` | recognised by its [marker](marker.md) |
| A second copy of a converted original that has something of its own 📝 | see [Second copies](#second-copies-of-converted-originals) |
| Immich has not finished reading it yet | it is picked up by a later run |
| It changed while it was being replaced | you edited it, or Immich was still analysing it. The copy is removed and a later run converts it |

## Choosing what to convert

You choose in Immich, with tags. Library mode creates them the first time it runs; they are under `immich-optimizer` in Immich's tag list.

| Tag | Set by | What the next run does |
|---|---|---|
| `immich-optimizer/convert` | you | converts the asset, then removes the tag |
| `immich-optimizer/keep` | you | keeps the asset as it is: marks it, without touching the file, and removes the tag. It is never converted, also if it is tagged `convert` later |
| `immich-optimizer/redo` | you | removes a `keep` mark (or the mark of an asset that was kept because converting it did not help), and removes the tag. Tag it `convert` too to have it converted in the same run |
| `immich-optimizer/failed` | the optimizer | the conversion failed: the reason is in the [audit log](audit-log.md) and the optimizer's own log. Tag it `convert` again to try once more |
| `immich-optimizer/review` | the optimizer | a [second copy](#second-copies-of-converted-originals) was left alone because it has something of its own: merge the two by hand |

**Tagging by hand.** In Immich, select the photos and videos, then choose **Tag** in the selection menu. "Select all" works in an album, in search results and on a whole day or month of the timeline, so:

- **To try library mode on a few photos**, put them in an album, open it, select all and tag them.
- **To convert everything from before a date**, select those months in the timeline, or search for them.
- **To convert your whole library**, select everything in the timeline and tag it. It takes a while in a very large library, but you only do it once.

**Tagging automatically.** Immich's own workflows (Administration → Workflows) can tag new assets as they arrive: for example "when an asset is uploaded, if it is a video, add the tag `immich-optimizer/convert`". A workflow can test the file name, the type, the date taken, the camera and other EXIF fields, the location, existing tags and the text Immich reads in a photo. Workflows react to new uploads, to metadata and text being read, and to tagging. They do not go through what is already in your library, and Immich has no workflow trigger for "added to an album": for those, tag by hand as above.

**Files converted by an earlier version.** Versions before 0.4 did not mark what they converted. Library mode cannot tell those files from originals, and would convert them a second time. Converting an already converted file almost never makes it smaller, and a result that is not smaller is never used, so nothing is lost, but it costs time. Tag such files `immich-optimizer/keep` to skip them. For example, if your earlier setup turned videos into `.mkv`, search Immich for `.mkv`, select all and tag them `keep`.

- **What a tag asks for happens once.** The tag is removed as soon as it is settled: converted, kept, marked, or skipped for good (no task handles its type, for example). It stays only when the reason may go away by itself, such as a file Immich has not finished reading: a later run tries again.
- **`keep` wins over `convert`.** An asset with both is kept as it is.
- **Tags are settled once a batch is finished.** A batch that is stopped (the container is restarted, for example) leaves them as they are, and they are acted on again after the restart.
- **The optimizer's tags are never carried over** to a converted copy, and are not counted as a difference when the copy is compared with its original. Your own tags are carried over as usual.
- **Cleaning up does not depend on tags.** [Second copies](#second-copies-of-converted-originals) are moved to the trash and replacements an earlier run left unfinished are completed, all over the library.

## Second copies of converted originals

An original can come back after it was converted: the mobile app uploads a photo again when the server no longer has a file with its checksum, which is the case once the original has left Immich's trash (see [Phones](#phones)).

Library mode recognises such a file without converting it again. Every converted asset carries a [marker](marker.md) with the checksum of its original, and each run reads them all. An unconverted asset whose file has that checksum is a second copy of the same photo.

- **It is moved to the trash**, and the audit log gets a `second_copy_trashed` entry that names both assets. The converted asset is not touched.
- **The optimizer keeps no list of its own.** It recognises second copies from the markers in Immich, afresh on every run.
- **It is left alone when it has something the converted asset lacks**: it is a favorite, has a description, a rating, tags or edits, or is in an album, stack or shared link the converted asset is not in. It is tagged `immich-optimizer/review`, and the audit log says what it has, so you can merge the two by hand.
- **It happens within seconds of the upload.** Immich announces every upload with its checksum, and library mode compares it with the originals its audit log knows. Nothing else is read.

## Phones

The official mobile app decides what to back up by checksum: a photo counts as backed up while the server has a file with the same checksum, in the timeline or in the trash. A converted file has another checksum.

**When the phone goes through the proxy, you can ignore this.** This is why library mode is best run together with proxy mode. A phone that reaches Immich through [proxy mode](proxy.md#the-mobile-app) is told the original's checksum together with each converted photo, whether the proxy or library mode converted it. So it keeps counting a photo as backed up after library mode has replaced it, also once the original has left the trash, and does not upload it again. You can tag the whole library for conversion without thinking about which photos are still on a phone. This is tested with the real Android app in an emulator: library mode converts photos the phone has, the trash is emptied, and the app uploads nothing.

**Without it**, a photo that is still on a phone with backup on comes back:

1. Library mode converts the photo and moves the original to the trash. The app still finds the original's checksum there and stays quiet.
2. Immich empties the trash (30 days by default). The checksum is gone, and the app uploads the photo again.
3. The next library run finds the [second copy](#second-copies-of-converted-originals) and moves it to the trash, which starts at 1 again.

Nothing is lost, but such a photo saves no space: its original is in the trash most of the time.

## Live photos, motion photos and edits

- **Live photos (Apple) and motion photos (Android)** are a photo paired with a short video. In Immich the video is an asset of its own, hidden behind the photo. Library mode converts the photo and pairs the copy with that same video. The video itself is not converted.
- **Edits made in Immich** are stored as positions on the original picture. They are copied to the new asset when it has the same pixel size. If your task resizes or crops, an edited asset fails the comparison and is left as it is; unedited assets are converted normally.

## Things to know before running it

- **A phone that still has the photos uploads them again** once the originals have left Immich's trash, unless it reaches Immich through the proxy. See [Phones](#phones).
- **Android: turn off "Sync remote deletions" in the Immich app** (Settings → Advanced, marked experimental, off by default) before a real run. With it on, the app deletes a photo from the phone when the same photo is moved to the trash on the server, which is exactly what library mode does with each original.
- **Space is freed when Immich empties its trash**, not at once. Until then the original also counts against a user's storage quota.
- It waits for Immich to finish reading each new file, so a large library takes a long time. By default it works on one asset at a time; [`IO_LIBRARY_CONCURRENCY`](configuration.md#library-mode) raises that, at the cost of more load on Immich. It can be stopped at any point: when it starts again it finishes or undoes whatever was in progress, and carries on with what is still tagged.

Every step is written to the [audit log](audit-log.md).

## Settings

See [Library mode settings](configuration.md#library-mode). The API key and Immich address are in [Immich settings](configuration.md#immich).
