# Development

## Layout

The code is split so that every way of receiving a file shares one conversion path.

| Path | What it is |
|---|---|
| `cmd/optimizer` | start-up, and the [settings table](../cmd/optimizer/settings.go) that defines every flag, environment variable and the settings reference |
| `internal/usecase/convert` | the conversion engine: picks the tasks for a file, limits how many run at once, decides between output and original, and owns the undone folder |
| `internal/usecase/pipeline` | watcher mode: folder events in, uploads out |
| `internal/adapter/proxy` | proxy mode: uploads, app sync for the mobile app, converting downloads |
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

## Tests

| Tests | Run with | In CI |
|---|---|---|
| [Unit tests](#unit-tests), with the [Immich API contract](#the-immich-api-contract) and the fuzz seeds | `go test -race ./...` | every push |
| [Fuzz tests](#fuzz-tests) | `go test -fuzz ...` | weekly |
| [Real motion photos](#real-motion-photos) | `IO_MOTIONPHOTO_SAMPLES=/path go test ...` | no |
| [Profile tests](#profile-tests), in the Docker image | `scripts/integration-test.sh [image]` | every push |
| [End-to-end tests](#end-to-end-tests), against a real Immich | `scripts/e2e-test.sh [image]` | every push |
| ... with [face detection](#face-detection) | `E2E_ML=1` | every push |
| ... with [video](#video) | `E2E_VIDEO=1` | when the profiles, the image or the test change, and always on `main` |
| ... with the [Android app](#the-android-app) | `E2E_ANDROID=1` | every push |
| ... with the [iOS app](#the-ios-app) | `E2E_IOS=1` | no: by hand, on a Mac |

The weekly [Newest Immich](../.github/workflows/immich-latest.yaml) workflow runs the API contract check and the end-to-end tests against Immich's newest release, so a change in Immich shows up before users upgrade into it.

### Unit tests

`go test ./...`. They include library mode against a fake Immich for every failure and resume path, and the watcher on real inotify.

- The inotify tests are Linux only. On a Mac, `scripts/linux-test.sh [packages] [flags]` runs them in a container.
- CI also runs `golangci-lint` and `govulncheck`. A vulnerability in the Go standard library is fixed by raising the Go patch version in `go.mod`.

### The Immich API contract

Runs with the unit tests. Every request the optimizer sends is checked against Immich's own description of its API, kept compact in `internal/adapter/immich/testdata/immich-api.json`: the path exists, and every query parameter and body field is one Immich knows and has not deprecated.

The comment at the top of `contract_test.go` says how to update it to another Immich version.

### Fuzz tests

For everything that reads bytes from clients or Immich: motion photos, upload forms, the sync stream, websocket frames and events, the marker.

- Their seed inputs run with the unit tests.
- The weekly [Fuzz](../.github/workflows/fuzz.yaml) workflow fuzzes each for a few minutes.
- To fuzz one locally:

  ```bash
  go test -run '^$' -fuzz '^FuzzSplit$' -fuzztime 1m -parallel 2 ./internal/adapter/motionphoto
  ```

- Go's fuzzer reports progress unevenly on macOS; on Linux it is steady.

### Real motion photos

`TestRealSamples` splits every motion photo in a folder and checks the result (needs ExifTool):

```bash
IO_MOTIONPHOTO_SAMPLES=/path go test -run RealSamples ./internal/adapter/motionphoto
```

Immich's own samples are in [immich-app/test-assets](https://github.com/immich-app/test-assets/tree/main/formats/motionphoto) (Pixel and Samsung, JPEG and HEIC). They are too large to keep in this repository.

### Profile tests

`scripts/integration-test.sh [image]` runs every bundled profile's real commands, inside the Docker image with the real tools, on the sample files in `cmd/optimizer/testdata/media`. The samples are synthetic. The default image is the published `:dev`.

It checks that:

- outputs decode;
- dates, GPS and Apple Live Photo identifiers survive;
- motion photos lose their video and their motion flag;
- a portrait video stays portrait;
- the lossless profile is really lossless;
- animations and multi-page files keep every frame and page;
- a result far smaller in pixels than its original is refused.

Notes:

- It uses this checkout's `config/`, so a profile change can be tested without rebuilding the image.
- CI runs it on every push against an image built from the commit.
- Locally the container gets 3 CPUs and 4 GB (`IO_TEST_CPUS`, `IO_TEST_MEMORY` change that), so the machine stays usable during the video encode.

### End-to-end tests

`scripts/e2e-test.sh [image]` starts a throwaway Immich with Docker Compose and runs the optimizer against it in library, proxy and watcher mode. It builds the optimizer from this checkout and mounts it over the image's binary. CI runs it on every push.

It covers:

- **Library mode:** a full replacement with everything carried over, a conversion that produces a flat image, a lossy task, interrupted replacements, the tags (convert, a profile, keep, failed), converting again, several users in one container, live tagging and second copies while library mode runs.
- **Proxy mode:** uploads with the marker and the undone folder, uploads from `immich-cli`, `immich-go` and through a shared link, app sync as a phone would see it, downloads converted back.
- **Watcher mode:** uploads.

The variants below add tests that need more than Immich itself. Each is switched on with an environment variable.

#### Where downloads and test devices go

The tests download a few things (the Immich Android app, the source of the iOS app, test videos, `immich-go`) and create test phones. By default these go to the system's cache and temp folders. To keep them on a disk of your choice, create a file `.dev.env` at the top of the checkout (it is not in git):

```sh
IO_DEV_DIR=/path/to/a/big/disk/immich-optimizer
ANDROID_HOME=/path/to/android/sdk
```

The test scripts read it, so neither has to be typed with each command.

#### Face detection

`E2E_ML=1 scripts/e2e-test.sh` also starts Immich's machine learning and runs the test that needs real face detection:

1. Three photos of the same face are recognised by Immich, and the person is named.
2. Library mode converts them.
3. Each copy must end up with that person exactly once, after Immich has processed the new files.

It downloads the machine-learning image and a model, so it takes a few minutes longer. CI runs it this way.

#### Video

`E2E_VIDEO=1 scripts/e2e-test.sh` also runs the video test:

1. It takes two real, public-domain phone clips from Immich's test assets: an iPhone and a Pixel portrait video, both with a position.
2. It converts them with `profile1`'s own command.
3. It uploads original and result to Immich and compares what Immich reads: date, time zone, position, camera and upright shape must be the same.

It downloads about 90 MB and transcodes 4K in software, so it takes minutes and all the CPU Docker has. CI runs it when the profiles, the image or the test change, and always on `main`.

#### The Android app

`E2E_ANDROID=1 scripts/e2e-test.sh` also boots an Android emulator, installs the real Immich app and drives it: it logs in through the proxy, backs up an album and reads the app's own database to see what the app considers backed up. This is the test for [app sync](app-sync.md). Add `-run TestAndroid` to run only this.

- It needs an Android SDK in `ANDROID_HOME` with the emulator and one system image: `sdkmanager "emulator" "platform-tools" "system-images;android-36;google_apis;arm64-v8a"` (`x86_64` instead of `arm64-v8a` on Intel/AMD, where it also needs KVM). The `google_apis` image is required, not `playstore`: reading the app's database takes root.
- `scripts/android-emulator.sh up` creates the device in a temporary folder, boots it without a window and installs the app (downloaded once from Immich's releases, `IMMICH_APP_VERSION` picks the version); `down` stops and deletes it. The test script does both by itself; an emulator that is already up is reused and left running, which saves the boot when running the test repeatedly.
- The device is only for these tests: every photo on it is deleted and the app's data is cleared before each case.
- The app is driven through Android's accessibility tree by the labels of its buttons, so a new app version that renames them needs the test updated.

#### The iOS app

`E2E_IOS=1 scripts/e2e-test.sh` does the same with the real Immich iOS app in Xcode's simulator, on a Mac. It is run by hand, not by CI. Add `-run TestIOS` to run only this; that takes about a quarter of an hour once the app is built.

Besides what the Android test checks, it covers what only an iPhone has: the iCloud record the app keeps for each asset, a Live Photo, a photo edited in the Photos app, and iOS's background task.

- It needs Xcode with an iOS simulator runtime, and Homebrew's `ruby` (for CocoaPods). Everything else is fetched by `scripts/ios-simulator.sh` into one folder (`ios` under `IO_DEV_DIR`) and nothing is installed system-wide.
- Immich publishes no build of its app for the simulator, so the first run builds it from Immich's source at `IMMICH_APP_VERSION`, with the tools Immich pins. Expect about 25 GB and half an hour or more. The build runs at background priority so the Mac stays usable; `IO_IOS_FULL_SPEED=1` lifts that.
- `scripts/ios-simulator.sh up` creates and boots the simulated iPhone, without a window, and installs the app; `reset` wipes it; `down` deletes it. The test script calls them by itself, and wipes the phone before the test. A phone that is already up is reused and left running.
- The simulated phone is kept on the internal disk (`~/Library/Caches/immich-optimizer-ios-simulators`, or `IO_IOS_DEVICES`) even when `IO_DEV_DIR` is on an external one: macOS does not let the simulator's own programs read an external volume.
- The screen is read and tapped with [idb](https://github.com/facebook/idb), by the labels of the app's buttons. Two things on screen cannot be read that way and are handled by position, for the simulated iPhone 16: iOS's "Save Password?" sheet, and the grid of the Photos app.
- The background task only runs when iOS decides to. In the simulator only the debugger can ask for it, and only on a Mac that lets the debugger attach without a password (`sudo DevToolsSecurity -enable`). Otherwise that part is skipped, and says so.

## Updates

Dependabot proposes updates of the Go modules, the GitHub Actions and the image's base images every week, against `dev`.

- **GitHub Actions** are pinned by commit and **the base images** by digest. Dependabot updates both.
- **The tools built into the image** are pinned in `Dockerfile.goreleaser` (`CAESIUM_VERSION`, `EXIFTOOL_VERSION`), each with the SHA-256 of its download next to it. They are updated by hand: change the checksum with the version, and run the profile tests after changing one.

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
