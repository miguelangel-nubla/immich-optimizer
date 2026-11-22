# How app sync works

This page explains what the proxy does so that [the mobile app](proxy.md#the-mobile-app) counts converted photos as backed up. You do not need it to use the proxy; it is here for when you want to know what happens, or have to find out why something does not.

## The problem

The mobile app keeps its own list of what is on the server, which Immich fills through a "sync". A photo on the phone counts as backed up when that list has an entry with the same checksum as the photo's file.

A converted photo is another file, with another checksum. The list has the converted one; the phone has the original. They never match, so the app uploads the photo again every time it looks for things to back up.

## What the proxy adds

Every converted asset carries a [marker](marker.md) in Immich, and the marker holds the checksum of the original. The marker is part of the same sync.

When a marker passes through the proxy on its way to the app, the proxy adds one more entry to what the app receives: a **hidden entry** with the original's checksum. The app finds its match and stops uploading the photo.

The hidden entry:

- **is never shown.** It is marked hidden, and as belonging to a library of its own, so it can sit next to an original that is still in Immich's trash.
- **does not replace anything.** The converted asset's own entry reaches the app unchanged.
- **exists only on the phone.** Nothing is added to Immich, and the proxy keeps no list either: it works each entry out from the marker Immich sends, every time. Its id is the converted asset's id with one character changed, so each can be found from the other.
- **stands in for the converted photo.** If the app ever asks for a hidden entry, for a thumbnail say, the proxy answers with the converted photo.

## Keeping it in step

- **Right after an upload.** Immich tells the app about a finished upload over a live connection (a websocket), not through the sync. The proxy adds the hidden entry there too, so the app counts the photo as backed up at once.
- **Trash and restore.** These change the asset, not its marker, so no marker passes by. For each asset the app is told about, the proxy therefore asks Immich whether it has a marker, and sends the hidden entry along in the same state: in the trash when the asset is, out of it when the asset is restored.
- **A marker that passes by alone.** The optimizer sometimes writes a marker again without the asset changing (the same original was uploaded once more, a replacement was completed). The marker says nothing about the trash, so the proxy asks Immich whether the asset is there before it sends the hidden entry. If Immich gives no answer, it sends none: an entry saying "not in the trash" for an asset that is there would make the app count a photo as safely backed up.
- **Deleting for good**, or removing the marker, deletes the hidden entry from the phone as well.
- **A phone that synced without the proxy**, or with an earlier version of it, has no hidden entries, and Immich has no reason to send those markers again. Such a phone uploads a photo once more. The proxy finds the conversion already in Immich, stores nothing new, and writes the marker again unchanged, which makes Immich send it out. The hidden entry arrives with the next sync.
- **Assets converted by library mode** carry the same marker, so all of this applies to them.

Two kinds of marker get no hidden entry: one on an original that library mode kept as it is (its own entry already has the right checksum), and one on a replacement library mode has not finished.

## Requests that name a hidden entry

The app finds "its" server asset for a photo through the checksum, which leads it to the hidden entry. Two things it then does name that entry in a request:

- **The iOS app records each photo's iCloud id** on the asset, for many assets in one request, each time it starts, for every asset that has no record yet. The proxy gives each hidden entry a copy of its asset's record in the sync, so the app has nothing to send for it. Should a request name a hidden entry anyway, the proxy takes it out: Immich would refuse the whole request over one id it does not know.
- **After a photo is edited on the phone** and uploaded again, the app stacks the new upload on the earlier one. The proxy puts the converted asset in the hidden entry's place, which is what the app means.

In the address of a request, a hidden id is replaced by the real one in the same way.

## What it costs

- **One more request to Immich per asset the app is told about**, to read its marker. In a sync with thousands of changes the proxy does this for the first 200 assets that are not in the trash; the others get their hidden entry from their marker, as usual. A marker whose asset was not in the same sync costs one request too, to learn whether the asset is in the trash.
- **The live connection is not compressed.** The proxy has to read the messages on it, so it asks Immich to send them as they are.
- **The app's acknowledgements are edited.** The app confirms every entry it received to Immich. The proxy removes the ones for hidden entries on the way, because Immich has never heard of them.

## What can go wrong

- **Immich changes the format** of the sync or of the live messages. The proxy passes on unchanged whatever it does not recognise, so the app would stop receiving hidden entries and go back to uploading. Nothing in Immich is affected.
- **The app reaches Immich some other way** for part of its traffic. A sync that does not go through the proxy carries no hidden entries.
- **The phone stops going through the proxy.** Hidden entries stay on it until the app syncs from scratch: log out and in again.

## What it is tested with

The [end-to-end tests](development.md) drive the real Immich apps (v3.4.2) through the proxy and read the app's own database to see what it considers backed up:

- **Android**, in an emulator, on every push: a backup with photos, a motion photo and a video, trash, restore and permanent deletion, library mode converting photos the phone still has, and what the app does when the proxy answers with an error.
- **iOS**, in Xcode's simulator, run by hand on a Mac: the same backup, trash, restore and deletion, plus the iCloud records above, a Live Photo (the photo converted, still paired with its video), and a photo edited in the Photos app.

Against a real Immich without a phone, they also cover a phone that had synced before, and the requests above.

Not run on iOS: **uploads in the background.** iOS starts the app's background task when it chooses, and the simulator only does it on demand through the debugger. The background task uses the same upload and the same check for what is backed up as the app in front, and names the file in a separate form field, which the proxy reads. RAW+JPEG pairs and burst photos are not in the test either; what the app uploads for those is up to iOS, with or without the proxy.
