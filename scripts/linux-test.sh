#!/usr/bin/env bash
# Runs the unit tests on Linux, in a container, from any machine: the watcher
# is built on inotify and its tests only run there.
#
#   scripts/linux-test.sh [packages...] [go test flags...]
#
# Without a package it runs everything. The container gets IO_TEST_CPUS
# (default 2) and IO_TEST_MEMORY (default 2g). Go's module and build caches
# are kept in Docker volumes between runs.
set -euo pipefail

cd "$(dirname "$0")/.."
GO_VERSION="$(sed -n 's/^go //p' go.mod)"
ARGS=("$@")
# Without a package (only flags, or nothing), everything.
HAS_PKG=
for a in "$@"; do
  case "$a" in ./*|github.com/*) HAS_PKG=1 ;; esac
done
if [ -z "$HAS_PKG" ]; then
  ARGS=(./... "$@")
fi

# The checkout is mounted read-only and copied inside: the tests write next to
# their sources (testdata), and the copy keeps that out of the checkout. The
# tests run as an ordinary user: some check what happens without permission.
# --init reaps what the tests kill (the processor's tests check that a task's
# whole process tree is gone). The arguments reach the inner shell quoted,
# whatever they contain.
QUOTED="$(printf '%q ' "${ARGS[@]}")"
docker run --rm --init --cpus "${IO_TEST_CPUS:-2}" --memory "${IO_TEST_MEMORY:-2g}" \
  -v "$PWD:/src:ro" -v io-test-gomod:/go/pkg/mod -v io-test-gocache:/cache \
  -e GOFLAGS=-buildvcs=false -e "TEST_ARGS=$QUOTED" \
  "golang:${GO_VERSION%.*}" bash -c '
    set -e
    useradd -m tester 2>/dev/null || true
    chown -R tester /go/pkg/mod /cache
    cp -r /src /home/tester/src && chown -R tester /home/tester/src
    cd /home/tester/src
    exec su tester -c "PATH=/usr/local/go/bin:\$PATH GOCACHE=/cache GOMODCACHE=/go/pkg/mod GOFLAGS=-buildvcs=false go test -race $TEST_ARGS"
  '
