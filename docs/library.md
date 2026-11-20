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

Everything it changes can be undone from Immich's trash, and nothing is changed until you say so: **it starts as a dry run.** Without `IO_LIBRARY_DRY_RUN=false` it only reports what it would convert.

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

The validity check in step 2 does **not** compare the picture with the original either: a task is free to crop, resize, watermark or compress lossily. It only rejects results that are broken. Library mode therefore converts with whatever your tasks file says, **including lossy tasks**: with a lossy profile, the only way back to the original quality is Immich's trash, for as long as it keeps the file.

## What is skipped

These assets are left exactly as they are. The reason for each is written to the debug log; assets marked 📝 also get an audit log entry.

| Asset | Why |
|---|---|
| Its task is a passthrough, or no task handles its type | there is nothing to convert |
| It does not get smaller 📝 | the original is kept and marked, so later runs do not download it again |
| Live photos and motion photos | a photo paired with a video is not handled yet |
| Edited in Immich (crop, rotate) | edits are not handled yet |
| Has comments or likes in a shared album | they belong to the asset and cannot be moved to the copy |
| In the locked folder | not visible to an API key |
| In an external library | Immich does not own those files |
| In the trash, hidden, or offline | not part of the visible library |
| Belongs to another user or a partner | each user runs library mode with their own key |
| Already converted, or already looked at | recognised by its [marker](marker.md) |
| Newer than `IO_LIBRARY_TAKEN_BEFORE` | your own limit |
| Immich has not finished reading it yet | it is picked up by a later run |

## Things to know before running it

- **The official mobile app will upload the originals again** for photos that are still on a phone with backup enabled, because the server no longer has a file with the checksum the app looks for. Use `IO_LIBRARY_TAKEN_BEFORE` to limit the run to photos that are no longer on your phone. See [Proxy mode limitations](proxy.md#limitations).
- **Space is freed when Immich empties its trash**, not at once. Until then the original also counts against a user's storage quota.
- It works through one asset at a time and waits for Immich to finish reading each new file, so a large library takes a long time. A run can be stopped at any point and started again: it continues where it left off, and finishes or undoes whatever was in progress.
- Start with a small batch (`IO_LIBRARY_LIMIT=20`) and look at the result in Immich before letting it run through everything.

Every step is written to the [audit log](audit-log.md).

## Settings

See [Library mode settings](configuration.md#library-mode). The API key and Immich address are in [Immich settings](configuration.md#immich).
