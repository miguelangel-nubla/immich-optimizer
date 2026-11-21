# Quick start

This sets up the recommended way to use the optimizer: every photo you upload to Immich is made smaller on the way in, **without losing any quality**. It takes about ten minutes.

You need an Immich that already runs with Docker Compose, and to be able to edit its `docker-compose.yml` file.

## What you get

- JPEG and PNG photos are stored as JPEG XL, usually about a fifth smaller. Not a single pixel changes.
- Videos and every other kind of file are stored as they are.
- You keep using Immich exactly as before, at the same address, in the browser and in the phone app.
- When you download a photo from Immich, you get a normal JPEG again.

## Set it up

**1. Open your Immich `docker-compose.yml`.**

**2. Find the `immich-server` part and delete its `ports:` lines.** They look like this:

```yaml
    ports:
      - '2283:2283'
```

The optimizer takes over that address, and passes everything on to Immich.

**3. Add the optimizer** to the same file, under `services:`, next to `immich-server`:

```yaml
  immich-optimizer:
    image: ghcr.io/miguelangel-nubla/immich-optimizer:latest
    environment:
      - IO_MODE=proxy
      - IO_IMMICH_URL=http://immich-server:2283
    ports:
      - '2283:2283'
    volumes:
      - ./optimizer-undone:/undone
    depends_on:
      - immich-server
    restart: unless-stopped
```

Indent it exactly like the other services. If your `immich-server` goes by another name in your file, use that name in the `IO_IMMICH_URL` line.

**4. Apply it.** In the folder of that file, run:

```
docker compose up -d
```

**5. Check it.** Open Immich in your browser as you always do and upload a JPEG photo. Open the photo, press the **i** button, and look at the file name: it now ends in `.jxl`. That is all.

## The phone app

Nothing changes in the app: it uses the same address as before.

The app would normally not recognise its own photos once they are converted, and upload them again. The optimizer takes care of that by itself, with the Android and the iPhone app. [More about it.](proxy.md#the-mobile-app)

## Good to know

- **Only new uploads are converted.** The photos already in your library stay as they are. To shrink those too, see [the next step](#the-next-step).
- **A photo that cannot be converted is not uploaded.** You get an error, and a copy is kept in the `optimizer-undone` folder next to your compose file, so nothing is lost. This is rare. [What to do with it.](undone.md)
- **To see what the optimizer is doing:** `docker compose logs immich-optimizer`.
- **To go back:** remove the `immich-optimizer` part, put the `ports:` lines back under `immich-server`, and run `docker compose up -d` again. The photos stored as JPEG XL stay in Immich and keep working.

## The next step

- **Shrink the photos you already have:** [Library mode](library.md). You pick the photos in Immich by giving them a tag, and it replaces each one with its smaller version, keeping the original in Immich's trash.
- **Convert more, or differently** (smaller files with some loss, videos): [Tasks](tasks.md).
- **Everything else:** the [documentation overview](../README.md#documentation).
