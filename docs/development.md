# Development

## Layout

The code is split so that every way of receiving a file shares one conversion path.

| Path | What it is |
|---|---|
| `cmd/optimizer` | start-up, and the [settings table](../cmd/optimizer/settings.go) that defines every flag, environment variable and the settings reference |
| `internal/usecase/convert` | the conversion engine: picks the tasks for a file, limits how many run at once, decides between output and original, and owns the undone folder |
| `internal/usecase/pipeline` | watcher mode: folder events in, uploads out |
| `internal/adapter/proxy` | proxy mode: uploads, the experimental app sync, converting downloads |
| `internal/usecase/library` | library mode |
| `internal/adapter/processor` | runs task commands |
| `internal/adapter/immich` | the Immich API |
| `internal/adapter/undone`, `internal/audit` | the undone folder and the audit log |
| `internal/adapter/motionphoto`, `internal/adapter/mediacheck` | motion photo splitting; the "is this a valid photo or video" check |
| `config/` | the bundled profiles |

A new source of files only needs to hand each file to the engine (`port.Converter`) and deliver the result.

## Build and test

```bash
go build -o immich-optimizer ./cmd/optimizer
go test -race ./...
scripts/build-image.sh            # builds immich-optimizer:local from this checkout
```

Three kinds of tests run in CI on every push:

- **Unit tests** (`go test ./...`), including library mode against a fake Immich for every failure and resume path, and the watcher on real inotify (Linux only). CI also runs `golangci-lint`.
- **Profile tests**, inside the Docker image with the real tools. `scripts/integration-test.sh [image]` runs every bundled profile's real commands on the sample files in `cmd/optimizer/testdata/media` inside the Docker image (default: the published `:dev` image) and checks the results: outputs decode, dates, GPS and Apple Live Photo identifiers survive, motion photos lose their video and their motion flag, a portrait video stays portrait, and the lossless profile is really lossless. It uses this checkout's `config/`, so a profile change can be tested without rebuilding the image. CI runs it on every push against an image built from the commit.

The samples are synthetic.
- **End-to-end tests**, against a real Immich. `scripts/e2e-test.sh [image]` starts a throwaway Immich with Docker Compose and runs the optimizer against it in library, proxy and watcher mode: a full library replacement with everything carried over, the dry run, a conversion that produces a flat image, a lossy task, interrupted runs, the library filter, second copies and scheduled runs, proxy uploads with the marker and the undone folder, uploads from `immich-cli`, `immich-go` and through a shared link, the experimental app sync as a phone would see it, downloads converted back, and watcher uploads. It builds the optimizer from this checkout and mounts it over the image's binary. CI runs it on every push.

`E2E_ML=1 scripts/e2e-test.sh` also starts Immich's machine learning and runs the test that needs real face detection: three photos of the same face are recognised by Immich, the person is named, library mode converts them, and each copy must end up with that person exactly once after Immich has processed the new files. It downloads the machine-learning image and a model, so it takes a few minutes longer. CI runs it this way.

`E2E_VIDEO=1 scripts/e2e-test.sh` also runs the video test. It takes two real, public-domain phone clips from Immich's test assets (an iPhone and a Pixel portrait video, both with a position), converts them with `profile1`'s own command, uploads original and result to Immich and compares what Immich reads: date, time zone, position, camera and upright shape must be the same. It downloads about 90 MB and transcodes 4K in software, so it takes minutes and all the CPU Docker has. CI runs it when the profiles, the image or the test change, and always on `main`.

`E2E_ANDROID=1 scripts/e2e-test.sh` also boots an Android emulator, installs the real Immich app and drives it: it logs in through the proxy, backs up an album and reads the app's own database to see what the app considers backed up. This is the test for the [experimental app sync](app-sync.md), with it off and on. Add `-run TestAndroid` to run only this.

- It needs an Android SDK in `ANDROID_HOME` with the emulator and one system image: `sdkmanager "emulator" "platform-tools" "system-images;android-36;google_apis;arm64-v8a"` (`x86_64` instead of `arm64-v8a` on Intel/AMD, where it also needs KVM). The `google_apis` image is required, not `playstore`: reading the app's database takes root.
- `scripts/android-emulator.sh up` creates the device in a temporary folder, boots it without a window and installs the app (downloaded once from Immich's releases, `IMMICH_APP_VERSION` picks the version); `down` stops and deletes it. The test script does both by itself; an emulator that is already up is reused and left running, which saves the boot when running the test repeatedly.
- The device is only for these tests: every photo on it is deleted and the app's data is cleared before each case.
- The app is driven through Android's accessibility tree by the labels of its buttons, so a new app version that renames them needs the test updated.

## Changing a setting

Edit the table in [`cmd/optimizer/settings.go`](../cmd/optimizer/settings.go), then regenerate the reference:

```bash
IO_UPDATE_DOCS=1 go test ./cmd/optimizer -run TestSettingsDocs
```

The test fails when [configuration.md](configuration.md) is out of date.

## Releases

- Pushing to `dev` publishes the `:dev` images.
- Pushing a tag publishes a release. A tag with a suffix such as `-beta.1` is a pre-release and does not move `:latest`.

## Contributing

1. Fork the repository and branch from `dev`.
2. Make your change, with tests.
3. Open a pull request against `dev`.
