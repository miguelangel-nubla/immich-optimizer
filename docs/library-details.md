# How library mode works

This page describes what [library mode](library.md) does to each asset: the order it works in, what the converted copy keeps, which assets it leaves alone, and how it handles an original that is uploaded again. You do not need it to use library mode; it is here for when you want to know what happens, or why an asset was not converted.

Every step described here is written to the [audit log](audit-log.md).

## How it knows what to do

Library mode keeps a live connection to Immich, the same one the web and mobile apps use.

- **When an asset is tagged**, Immich tells it within a second or two, and it acts on it.
- **Every 15 minutes** it also looks for tagged assets by itself, in case a message was missed.
- **When it starts**, it first settles anything left unfinished (a replacement [interrupted](#if-it-is-interrupted) by a restart, for example) and then acts on whatever is tagged.

To know what may be unfinished, and which originals were converted into what, it reads its [audit log](audit-log.md) instead of every asset in Immich. The log is only used to know where to look: before acting, the optimizer always checks the asset's [marker](marker.md) in Immich. If the log is missing or empty, it reads the whole library once instead, which takes a while on a large library.

## What happens to each asset

Immich has no "replace this file", so the optimizer does it in a fixed order that never leaves you without your photo:

1. **Download** the original and check it against Immich's checksum.
2. **Convert** it with your tasks, then check that the result [looks like a valid photo or video](#the-validity-check).
3. **Upload** the converted file as a new asset.
4. **Carry everything over**: albums, shared links, memories, stack position, favorite, archive state, description, rating, date and location edits, tags, people (named faces) and the mobile app's own metadata.
5. **Compare** the new asset with the original, field by field, and check that nothing was added to the original meanwhile: an album, a shared link, a person, or comments and likes in a shared album.
6. **Only if everything matches**, mark the new asset as verified and move the original to Immich's **trash**.

If anything fails or differs before step 6, the new asset is deleted again and the original is left exactly as it was. A change made to the original meanwhile only means it is converted at a later pass.

Because the original goes to the trash, you can restore it from Immich for as long as your trash keeps it (30 days by default). The space is freed when the trash is emptied.

### The validity check

The result of a conversion must look like a valid photo or video:

- The file is not empty.
- It decodes completely.
- It is not a single flat colour, unless the original was.
- For a video: it has a picture track, a length, and a frame from the middle decodes.

The check does **not** compare the picture with the original: a task is free to crop, resize, watermark or re-encode. It only rejects results that are broken. Library mode converts with whatever your tasks file (or the [profile](library.md#profiles) the asset is tagged for) says, and treats every task the same way.

### If it is interrupted

After a restart or a lost connection, the next start settles each unfinished replacement from the new asset's [record](marker.md). What it does depends on how far the replacement got:

| The copy | Its original | What happens |
|---|---|---|
| not verified yet | still in place | the copy is deleted, and the original is converted afresh |
| not verified yet | in the trash anyway | the copy is kept and tagged `immich-optimizer/review` |
| verified | in the trash | nothing: the swap is complete |
| verified | back in place (you restored it, or the move to the trash did not reach Immich) | the swap is completed when the two still match. Otherwise both are kept and the copy is tagged `immich-optimizer/review` |

A verified copy is never deleted.

This does not depend on tags: an interrupted replacement is completed or undone when library mode starts, whether the asset is still tagged or not.

## What is carried over, and what is not

The converted copy is a new asset in Immich. This is everything the original had, and what happens to it.

**Carried over**, and checked in step 5:

| What | How |
|---|---|
| Albums (including shared ones) | copied by Immich |
| Individual shared links | copied by Immich, completed by the optimizer |
| Stack membership, and being the stack's cover | copied by Immich; cover set by the optimizer |
| Favorite, archived state | set on upload |
| Description, rating | copied by Immich with the sidecar; set by the optimizer if they still differ |
| Date and location you edited in Immich | same |
| Tags | same |
| People (faces assigned to a person) | Immich detects them again in the new file; any still missing are recreated by the optimizer with the same face box |
| File dates, date taken, time zone, GPS, camera and lens, dimensions, orientation | read by Immich from the converted file, then compared |
| Edits made in Immich (crop, rotate, mirror) | copied by the optimizer. Only possible when the conversion keeps the picture's size, see [edits](library.md#live-photos-motion-photos-and-edits) |
| The video of a live photo or motion photo | the copy is paired with the same video, which is left as it is |
| Asset metadata written by apps (for example the mobile app's iCloud id) | copied by the optimizer |
| Membership in memories ("on this day") | added by the optimizer |
| Owner, type, file name (with the new extension) | by upload |

**Made again by Immich**, so it may differ:

| What | What happens |
|---|---|
| Faces not assigned to anyone | not copied: Immich detects them again |
| A face you removed by hand, or hid | may come back when Immich detects faces again |
| Thumbnails, search index, face data | generated again by Immich for the new file |

**Lost:**

| What | What happens |
|---|---|
| The asset's id | the copy has a new one: links and bookmarks to the old asset stop working |
| The upload date ("recently added") | becomes the day of the conversion. Immich sets it itself and its API has no way to change it |
| Being an album's cover or a person's featured photo | Immich picks another one |

### What the comparison allows

If the copy differs from the original in anything of the first table, it is deleted and the original stays. A few allowances are made on purpose:

- The file name is compared without its extension.
- The copy may belong to more people than the original, never fewer.
- Memories are not read back: adding the copy to a memory is confirmed by Immich's answer, and a refusal undoes the replacement like any other difference.
- **Dimensions and orientation are not compared**, because a task may resize, crop or rotate.
- A video's length may differ by half a second or 2%.

## What is skipped

Library mode only looks at what is tagged to be converted. A tagged asset is still left exactly as it is in the cases below. The log says why, and those marked 📝 also get an [audit log](audit-log.md) entry.

### The `convert` tag is removed

There is nothing more to do for these:

| Asset | Why |
|---|---|
| No task converts its type: the task for it uploads it as it is, or there is none | there is nothing to convert. For a photo already converted, this is the usual case: the bundled profiles have no task for JPEG XL |
| Tagged `keep` as well | `keep` wins. Once you remove `keep`, tag it `convert` again |
| It does not get smaller 📝 | the original is kept and marked as kept. Tagging it again tries again, with your tasks as they are then |
| An asset that is already converted, and converting it again does not make it smaller | it stays exactly as it is, record included |
| A motion photo whose video Immich did not extract 📝 | converting the photo would lose the video |
| A motion photo with `motion_photos: keep` or `drop_photo` 📝 | the result would hide or drop a part of it. Use `drop_video`, as the bundled profiles do |
| Has comments or likes in a shared album | they belong to the asset and cannot be moved to the copy |
| In an external library | Immich does not own those files |
| Its record is from an earlier version and does not say what its original was | the new record could not tell phones which file they have |
| A second copy of a converted original that has something of its own 📝 | see [Second copies](#second-copies-of-converted-originals) |

### The tag stays

These are tried again later:

| Asset | When it is tried again |
|---|---|
| Tagged `failed` (its conversion failed) | once you remove `failed` |
| Immich has not finished reading it yet | at the next batch |
| In the trash, or its file is offline | once it is back |
| It changed while it was being replaced (you edited it, or Immich was still analysing it) | the copy is removed, and it is converted at the next batch |

### Not seen at all

A tag on these does nothing:

- Photos that are hidden or in the locked folder.
- Other people's photos (a partner's, or someone else's in a shared album). A key acts on its own user's photos only: the owner's key converts them.

## Second copies of converted originals

An original can come back after it was converted: the mobile app uploads a photo again when the server no longer has a file with its checksum, which is the case once the original has left Immich's trash (see [Phones](library.md#phones)).

Library mode recognises such a file without converting it again. Every converted asset carries a [marker](marker.md) with the checksum of its original. An unconverted asset whose file has that checksum is a second copy of the same photo.

**What happens to a second copy:**

- **It is moved to the trash**, and the audit log gets a `second_copy_trashed` entry that names both assets. The converted asset is not touched.
- **It is left alone when it has something the converted asset lacks**: it is a favorite, has a description, a rating, tags or edits, or is in an album, stack or shared link the converted asset is not in. It is tagged `immich-optimizer/review`, and the audit log says what it has, so you can merge the two by hand.
- **This does not depend on tags.** A second copy is handled whether anything is tagged or not.

**How it is found:**

- **Within seconds of the upload.** Immich announces every upload with its checksum, and library mode compares it with the originals its audit log knows. Nothing else is read.
- **Afterwards, for uploads made while library mode was not listening.** Immich announces an upload only to who is connected at that moment. When library mode starts, when its connection to Immich comes back, and every 15 minutes, it asks Immich for the assets that arrived since it last looked and compares their checksums the same way. That is one search, not a read of the library.
- **Without a list of its own.** The [audit log](audit-log.md) says which originals were converted into what, and before acting the optimizer checks the converted asset's marker in Immich. With no audit log, it reads the markers of the whole library once.
