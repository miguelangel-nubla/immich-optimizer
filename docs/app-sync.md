# How app sync works

This page explains what the proxy does when [`IO_PROXY_EXPERIMENTAL_APP_SYNC=true`](proxy.md#the-mobile-app). You do not need it to use the setting; it is here for when you want to know what happens, or have to find out why something does not.

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
- **Deleting for good**, or removing the marker, deletes the hidden entry from the phone as well.
- **A phone that synced before the setting was on** has no hidden entries, and Immich has no reason to send those markers again. Such a phone uploads a photo once more. The proxy finds the conversion already in Immich, stores nothing new, and writes the marker again unchanged, which makes Immich send it out. The hidden entry arrives with the next sync.
- **Assets converted by library mode** carry the same marker, so all of this applies to them.

Two kinds of marker get no hidden entry: one on an original that library mode kept as it is (its own entry already has the right checksum), and one on a replacement library mode has not finished.

## What it costs

- **One more request to Immich per asset the app is told about**, to read its marker. In a sync with thousands of changes the proxy does this for the first 200 assets that are not in the trash; the others get their hidden entry from their marker, as usual.
- **The live connection is not compressed.** The proxy has to read the messages on it, so it asks Immich to send them as they are.
- **The app's acknowledgements are edited.** The app confirms every entry it received to Immich. The proxy removes the ones for hidden entries on the way, because Immich has never heard of them.

## What can go wrong

- **Immich changes the format** of the sync or of the live messages. The proxy passes on unchanged whatever it does not recognise, so the app would stop receiving hidden entries and go back to uploading. Nothing in Immich is affected.
- **The app reaches Immich some other way** for part of its traffic. A sync that does not go through the proxy carries no hidden entries.
- **The setting is turned off again.** Hidden entries stay on a phone until the app syncs from scratch: log out and in again.

## What it is tested with

The [end-to-end tests](development.md) drive the real Immich Android app (v3.4.2) in an emulator: a backup with and without the setting, photos, a motion photo and a video, trash, restore and permanent deletion, turning the setting on for a phone that had already synced, and library mode converting photos the phone still has. It has not been tried on iOS.
