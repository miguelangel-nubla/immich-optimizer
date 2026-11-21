#!/usr/bin/env bash
# End-to-end tests: starts a throwaway Immich with Docker Compose and runs the
# optimizer image against it in library, proxy and watcher mode.
#
#   scripts/e2e-test.sh [image] [go test flags...]
#
# The image defaults to the published dev build, but the optimizer binary and
# the profiles are always taken from this checkout (the binary is built here
# and mounted over the image's), so the tests exercise the code you have.
# Set E2E_KEEP=1 to leave Immich running afterwards.
# Set E2E_ML=1 to also start Immich's machine learning and run the tests that
# need real face detection (downloads the ML image and models: slow).
# Set E2E_VIDEO=1 to also run the video test: it converts two real phone clips
# with the transcoding profile and compares what Immich reads from them. It
# downloads about 90 MB and transcodes 4K in software, so it is slow and
# uses every core Docker has.
# Set E2E_ANDROID=1 to also boot an Android emulator with the Immich app and
# run the tests that drive the real app (see scripts/android-emulator.sh for
# what it needs). An emulator that is already up is reused and left running.
set -euo pipefail

IMAGE="${1:-ghcr.io/miguelangel-nubla/immich-optimizer:dev}"
shift || true

cd "$(dirname "$0")/.."
export E2E_PORT="${E2E_PORT:-12383}"
COMPOSE=(docker compose -f e2e/docker-compose.yml)
if [ -n "${E2E_ML:-}" ]; then
  COMPOSE+=(--profile ml)
  export E2E_ML_ENABLED=true
fi
OUT="$(mktemp -d)"

ANDROID_SERIAL=""
ANDROID_STARTED=""

cleanup() {
  if [ -n "$ANDROID_STARTED" ] && [ -z "${E2E_KEEP:-}" ]; then
    scripts/android-emulator.sh down || true
  fi
  if [ -z "${E2E_KEEP:-}" ]; then
    "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  fi
  rm -r "$OUT"
}
trap cleanup EXIT

ARCH="$(docker image inspect --format '{{.Architecture}}' "$IMAGE" 2>/dev/null || true)"
if [ -z "$ARCH" ]; then
  docker pull "$IMAGE" >/dev/null
  ARCH="$(docker image inspect --format '{{.Architecture}}' "$IMAGE")"
fi
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -ldflags "-X main.version=e2e" -o "$OUT/immich-optimizer" ./cmd/optimizer

"${COMPOSE[@]}" up -d
echo "Waiting for Immich on port $E2E_PORT..."
for _ in $(seq 1 120); do
  if curl -fs -m 3 "http://127.0.0.1:$E2E_PORT/api/server/ping" >/dev/null 2>&1; then break; fi
  sleep 3
done
curl -fs -m 5 "http://127.0.0.1:$E2E_PORT/api/server/ping" >/dev/null || { "${COMPOSE[@]}" logs --tail 40 immich-server; exit 1; }

if [ -n "${E2E_ANDROID:-}" ]; then
  ANDROID_SERIAL="$(scripts/android-emulator.sh serial)"
  if ! "$ANDROID_HOME/platform-tools/adb" devices | grep -q "^$ANDROID_SERIAL[[:space:]]*device"; then
    ANDROID_STARTED=1
    scripts/android-emulator.sh up
  fi
fi

E2E_ANDROID_SERIAL="$ANDROID_SERIAL" \
E2E_ADB="${ANDROID_HOME:+$ANDROID_HOME/platform-tools/adb}" \
E2E_IMMICH_URL="http://127.0.0.1:$E2E_PORT" \
E2E_IMMICH_INTERNAL_URL="http://immich-server:2283" \
E2E_NETWORK="io-e2e_default" \
E2E_IMAGE="$IMAGE" \
E2E_BINARY="$OUT/immich-optimizer" \
E2E_CONFIG_DIR="$PWD/config" \
E2E_MEDIA_DIR="$PWD/cmd/optimizer/testdata/media" \
E2E_ML="${E2E_ML:-}" \
E2E_REPORT_COPY="${E2E_REPORT_COPY:-}" \
  go test -tags e2e -count=1 -timeout 30m ./e2e/ "$@"
