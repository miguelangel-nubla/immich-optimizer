# Library mode

Proxy and watcher mode only handle new uploads. Library mode (`IO_MODE=library`) converts the assets **already in Immich**: it replaces each one with its converted copy and moves the original to Immich's trash.

**You choose what is converted in Immich itself, with a tag.** Nothing happens until you tag something `immich-optimizer/convert`.

This page is about using it. What it does to each asset, step by step, is in [How library mode works](library-details.md).

## Before you start

- **Phones that back up with the Immich app: run library mode together with [proxy mode](proxy.md)**, and have the phones reach Immich through the proxy. Otherwise every converted photo that is still on a phone is uploaded again once its original has left Immich's trash. See [Phones](#phones).
- **Android: turn off "Sync remote deletions" in the Immich app** (Settings → Advanced, marked experimental, off by default) before you tag anything. With it on, the app deletes a photo from the phone when the same photo is moved to the trash on the server, which is exactly what library mode does with each original.
- **Space is freed when Immich empties its trash** (after 30 days by default), not at once. Until then the original can be restored from the trash, and also counts against the user's storage quota.
- **A large library takes a long time**, because library mode waits for Immich to finish reading each new file. By default it works on one asset at a time; [`IO_LIBRARY_CONCURRENCY`](configuration.md#library-mode) raises that, at the cost of more load on Immich.
- **It can be stopped at any point.** When it starts again it finishes or undoes whatever was in progress, and carries on with what is still tagged.

## Set it up

The recommended setup is proxy and library mode in one container. The [quick start](quickstart.md) covers the proxy part.

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

**On Linux, create the folder first:** `mkdir -p undone && sudo chown 1001 undone`. The optimizer does not start with a folder it cannot write to ([why](docker.md#folders-and-ports)).

In this setup the proxy converts new uploads as they arrive. To convert nothing until it is tagged, also set `IO_PROXY_CONVERT_UPLOADS=false` ([Converting only on demand](proxy.md#converting-only-on-demand)).

**To try it**, tag three or four photos `immich-optimizer/convert`. They are converted within seconds. Each converted photo is a new asset, and its original is in the trash, where you can compare the two or restore the original.

## API keys

Immich lets a key act on its owner's photos only, and has no key that works for every user. So each user whose photos should be converted creates a key once, in Immich under Account Settings → API Keys, with **all permissions**, and gives it to you.

- **Give one key per user**, all to one container. Separate them with commas in `IO_IMMICH_API_KEY`, or put one per line in the file named by `IO_IMMICH_API_KEY_FILE`. In the file, blank lines and lines starting with `#` are ignored, so you can note whose each key is.
- **Each key must be of a different user.** Two keys of the same user would work on the same photos at the same time, so the optimizer stops at startup with an error naming the user.
- **Each user is handled separately.** A user's tags act on that user's photos only.
- **All users share** the limit on conversions running at the same time ([`IO_TASKS_CONCURRENCY`](configuration.md#tasks)) and one [audit log](audit-log.md), in which each library entry names its user.
- **Watcher mode uploads with the first key.** Watcher mode on its own takes only one key.

In the log:

- At start, each key logs whose it is: `Library: working for Alice <alice@example.com> (user <id>)`.
- Each user's lines start with `[user <id>]`, the user's id in Immich. It stays the same when a key is replaced.
- Until Immich has said whose a key is (while Immich cannot be reached, for example), its lines say `[key 2 of 3]`.

## Choosing what to convert

You choose in Immich, with tags. Library mode creates them the first time it runs; they are under `immich-optimizer` in Immich's tag list.

| Tag | Set by | What happens |
|---|---|---|
| `immich-optimizer/convert` | you | The asset is converted with your tasks file, and the tag is removed. An asset that is already converted is [converted again](#converting-again) |
| `immich-optimizer/convert/<profile>` | you | The same, with that [profile](#profiles) instead of your tasks file |
| `immich-optimizer/keep` | you | Protects the asset: while it has this tag it is not converted, whatever else it is tagged. Remove the tag to end it. Nothing is written to the asset |
| `immich-optimizer/failed` | the optimizer | The conversion failed. The reason is in the [audit log](audit-log.md) and in the optimizer's own log. **Remove `failed` to try again** |
| `immich-optimizer/review` | the optimizer | Look at this asset. It is a [second copy](library-details.md#second-copies-of-converted-originals) that has something of its own, or a replacement that was [interrupted](library-details.md#if-it-is-interrupted) and could not be completed as planned. The log says which |

### Tagging by hand

In Immich, select the photos and videos, then choose **Tag** in the selection menu. "Select all" works in an album, in search results and on a whole day or month of the timeline, so:

- **To try library mode on a few photos**, put them in an album, open it, select all and tag them.
- **To convert everything from before a date**, select those months in the timeline, or search for them.
- **To convert your whole library**, select everything in the timeline and tag it. It takes a while in a very large library, but you only do it once.

### Tagging automatically

Immich's own workflows (Administration → Workflows) can tag new assets as they arrive: for example "when an asset is uploaded, if it is a video, add the tag `immich-optimizer/convert`".

- A workflow can test the file name, the type, the date taken, the camera and other EXIF fields, the location, existing tags and the text Immich reads in a photo.
- Workflows react to new uploads, to metadata and text being read, and to tagging.
- They do not go through what is already in your library, and Immich has no workflow trigger for "added to an album". For those, tag by hand.

### Protecting what should stay as it is

`convert` converts whatever it is on, also a photo that is already converted, so tagging in bulk includes those. Tag what must stay as it is `immich-optimizer/keep` first.

For example, versions before 0.4 did not mark what they converted. If your earlier setup turned videos into `.mkv`, search Immich for `.mkv`, select all and tag them `keep`.

### How tags behave

- **A tag is acted on once.** `convert` is removed as soon as the asset is settled: converted, kept as it is because converting did not make it smaller, or skipped for good (no task handles its type, for example).
- **`convert` stays** when the reason may go away by itself, such as a file Immich has not finished reading, and when the conversion failed. A failed asset is not tried again while it has `failed`.
- **Tagging again tries again.** A photo kept as it is because converting did not help is converted again when you tag it again, with your tasks as they are then.
- **`keep` wins** over every `convert` tag.
- **A profile wins over plain `convert`.** A photo tagged `convert` and `convert/<profile>` is converted with the profile. Two different profiles on one photo is a mistake: it is marked `failed`, and the log names both.
- **Change a photo's tags one at a time.** Immich can lose one of two changes made to the same photo within a second or two, such as removing one tag and adding another straight after. Add or remove several tags in one go (select them together in the tag picker), or wait a moment in between.
- **Tags are settled once a batch is finished.** A batch that is stopped (the container is restarted, for example) leaves them as they are, and they are acted on again after the restart.
- **The optimizer's tags are never carried over** to a converted copy, and are not counted as a difference when the copy is compared with its original. Your own tags are carried over as usual.

**The `immich-optimizer` tags belong to the optimizer.** At every start, library mode creates the ones it uses and deletes the ones it does not, such as tags of an earlier version or of a profile you removed, so the tag picker only offers what works. A tag that some photos still have is not deleted, since that would silently drop what was asked of them: the log names it and how many photos have it. Every tag library mode adds, removes, creates or deletes is in the [audit log](audit-log.md).

**A tagged asset that is not converted** was skipped for a reason, which the log gives. The cases are listed in [What is skipped](library-details.md#what-is-skipped).

### Converting again

Tagging a photo that is already converted `convert` (or `convert/<profile>`) converts it again, **from the converted file** it has now, for example with a task that makes it smaller still.

Your tasks (or the profile) need a task for the converted file's type for that: JPEG XL, for example. Without one, the tag is removed and nothing happens. The bundled profiles pass JPEG XL through as it is.

- The previous copy goes to the trash, like any original, and the new one takes its place with everything carried over.
- The result is used only when it is smaller. Otherwise the photo, and its record, stay exactly as they are.
- Its [record](marker.md) keeps every version. The first is still the original your phone has, so the phone keeps counting the photo as backed up.
- With a lossy task, quality is lost a second time.

## Profiles

A profile is another set of tasks, for converting some photos differently. Tag a photo `immich-optimizer/convert/<profile>` to convert it with that profile.

- **The bundled profiles** are available as they are: `convert/lossless` and `convert/profile1` (see [Bundled profiles](tasks.md#bundled-profiles)).
- **Your own** go in the profiles folder. Each profile is a folder with a `tasks.yaml`, and any scripts or presets its commands use, like the bundled ones. Mount the folder that holds them at `/etc/immich-optimizer/profiles` (or set [`IO_LIBRARY_PROFILES_DIR`](configuration.md#library-mode)). A folder named like a bundled profile replaces it.

  ```yaml
      volumes:
        - ./profiles:/etc/immich-optimizer/profiles:ro   # ./profiles/smaller/tasks.yaml
  ```

- **Restart after adding a profile.** Library mode creates one tag per profile when it starts, so they are in Immich's tag picker.
- **A profile's name** becomes part of a tag: letters, digits, `.`, `_` and `-` only.
- **Profiles are for library mode.** Uploads through the proxy and the watcher use your tasks file.
- All profiles share the limit on conversions running at the same time ([`IO_TASKS_CONCURRENCY`](configuration.md#tasks)).

## Phones

The official mobile app decides what to back up by checksum: a photo counts as backed up while the server has a file with the same checksum, in the timeline or in the trash. A converted file has another checksum.

### Through the proxy

**When the phone goes through the proxy, you can ignore this.** A phone that reaches Immich through [proxy mode](proxy.md#the-mobile-app) is told the original's checksum together with each converted photo, whether the proxy or library mode converted it. So it keeps counting a photo as backed up after library mode has replaced it, also once the original has left the trash, and does not upload it again.

You can tag the whole library for conversion without thinking about which photos are still on a phone.

This is tested with the real Android app in an emulator: library mode converts photos the phone has, the trash is emptied, and the app uploads nothing.

### Without the proxy

A photo that is still on a phone with backup on comes back:

1. Library mode converts the photo and moves the original to the trash. The app still finds the original's checksum there and stays quiet.
2. Immich empties the trash (30 days by default). The checksum is gone, and the app uploads the photo again.
3. Library mode sees the [second copy](library-details.md#second-copies-of-converted-originals) arrive and moves it to the trash, which starts at 1 again.

Nothing is lost, but such a photo saves no space: its original is in the trash most of the time.

## Live photos, motion photos and edits

- **Live photos (Apple) and motion photos (Android, JPEG or HEIC)** are a photo paired with a short video. In Immich the video is an asset of its own, hidden behind the photo. Library mode converts the photo and pairs the copy with that same video. The video itself is not converted.
- **Samsung files with only Samsung's own motion-photo index** (none seen yet) are not detected; see [Samsung motion photos](tasks.md#samsung-motion-photos).
- **Edits made in Immich** (crop, rotate, mirror) are stored as positions on the original picture. They are copied to the new asset when it has the same pixel size. If your task resizes or crops, an edited asset fails the comparison and is left as it is; unedited assets are converted normally.

## Settings

See [Library mode settings](configuration.md#library-mode). The API key and Immich address are in [Immich settings](configuration.md#immich).
