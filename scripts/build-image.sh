#!/usr/bin/env bash
# Builds the optimizer image from this checkout for the local platform, the
# same way a release does: the binary is built first and copied in.
#
#   scripts/build-image.sh [tag]        (default tag: immich-optimizer:local)
set -euo pipefail

TAG="${1:-immich-optimizer:local}"
cd "$(dirname "$0")/.."

ARCH="$(docker version --format '{{.Server.Arch}}')"
CTX="$(mktemp -d)"
trap 'rm -r "$CTX"' EXIT

mkdir -p "$CTX/linux/$ARCH"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$CTX/linux/$ARCH/immich-optimizer" ./cmd/optimizer
cp -R config docker LICENSE "$CTX/"
docker build --platform "linux/$ARCH" -f Dockerfile.goreleaser -t "$TAG" "$CTX"
echo "Built $TAG"
