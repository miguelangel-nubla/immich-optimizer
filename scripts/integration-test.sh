#!/usr/bin/env bash
# Runs the bundled-profile integration tests inside the optimizer image, where
# the real conversion tools are installed.
#
#   scripts/integration-test.sh [image] [go test flags...]
#
# The image defaults to the published dev build. The tests are compiled here
# for the image's architecture and run against this checkout's config/ and
# sample files, so profile changes are tested without rebuilding the image.
set -euo pipefail

IMAGE="${1:-ghcr.io/miguelangel-nubla/immich-optimizer:dev}"
shift || true

cd "$(dirname "$0")/.."
OUT="$(mktemp -d)"
trap 'rm -r "$OUT"' EXIT

ARCH="$(docker image inspect --format '{{.Architecture}}' "$IMAGE" 2>/dev/null || true)"
if [ -z "$ARCH" ]; then
  docker pull "$IMAGE" >/dev/null
  ARCH="$(docker image inspect --format '{{.Architecture}}' "$IMAGE")"
fi

CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go test -tags integration -c -o "$OUT/profiles.test" ./cmd/optimizer

# /dev/dri is passed through when present so the GPU video task can run.
DEVICES=()
if [ -e /dev/dri/renderD128 ]; then
  DEVICES=(--device /dev/dri)
fi

# Video encodes take every core they are given: the container gets
# IO_TEST_CPUS of them (default 3) and IO_TEST_MEMORY (default 4g), so the
# machine stays usable. CI sets them higher.
docker run --rm ${DEVICES[@]+"${DEVICES[@]}"} \
  --cpus "${IO_TEST_CPUS:-3}" --memory "${IO_TEST_MEMORY:-4g}" \
  -v "$OUT/profiles.test:/profiles.test:ro" \
  -v "$PWD/config:/repo/config:ro" \
  -v "$PWD/cmd/optimizer/testdata/media:/repo/media:ro" \
  -e IO_TEST_CONFIG_DIR=/repo/config \
  -e IO_TEST_MEDIA_DIR=/repo/media \
  --entrypoint /profiles.test \
  "$IMAGE" -test.run '^TestIntegration' -test.v "$@"
