#!/usr/bin/env bash
# Builds the Immich iOS app for the simulator and brings a simulated iPhone
# with it up or down, for the phone end-to-end tests
# (E2E_IOS=1 scripts/e2e-test.sh). macOS only.
#
#   scripts/ios-simulator.sh build   get the tools and build the app (slow the first time)
#   scripts/ios-simulator.sh up      build if needed, create the device, boot it, start the UI helper
#   scripts/ios-simulator.sh down    stop the helper and delete the device
#   scripts/ios-simulator.sh reset   wipe the device (photos, apps), install the app again
#   scripts/ios-simulator.sh ready   succeeds if the device is booted and the helper answers
#   scripts/ios-simulator.sh env     print the E2E_IOS_* variables the tests read
#
# Immich publishes no build of its app for the simulator, so it is built from
# source at IMMICH_APP_VERSION. Everything that takes goes into one folder
# (IO_IOS_DIR, by default "ios" under IO_DEV_DIR): Immich's source, and the
# tools at the versions Immich pins (Flutter, Rust, Java, Node, CocoaPods),
# installed there with mise. Nothing is installed system-wide. Expect about
# 25 GB and, the first time, half an hour or more.
#
# It needs: Xcode with an iOS simulator runtime, and a Ruby newer than the
# one macOS ships (Homebrew's "ruby" formula), for CocoaPods.
#
# The simulated phone itself is kept on the internal disk (IO_IOS_DEVICES):
# macOS does not let the simulator's own programs read an external volume.
# The UI is driven with idb (github.com/facebook/idb), also kept in the folder.
set -euo pipefail

. "$(dirname "$0")/dev-env.sh"
[ "$(uname)" = Darwin ] || { echo "the iOS simulator only exists on macOS" >&2; exit 1; }

APP_VERSION="${IMMICH_APP_VERSION:-v3.4.2}"
IDB_VERSION="${IO_IDB_VERSION:-v1.7.8}"
DIR="${IO_IOS_DIR:-${IO_DEV_DIR:+$IO_DEV_DIR/ios}}"
DIR="${DIR:-$HOME/Library/Caches/immich-optimizer-ios}"
DEVICES="${IO_IOS_DEVICES:-$HOME/Library/Caches/immich-optimizer-ios-simulators}"
DEVICE_NAME="io-e2e"
DEVICE_TYPE="${IO_IOS_DEVICE_TYPE:-iPhone 16}"
PORT="${IO_IDB_PORT:-10882}"
BUNDLE="app.futo.immich.debug"
SRC="$DIR/immich"
APP="$SRC/mobile/build/ios/iphonesimulator/Immich-Debug.app"
IDB="$DIR/idb/venv/bin/idb"
COMPANION="$DIR/idb/idb_companion"

# The tools keep to the folder.
export MISE_DATA_DIR="$DIR/mise/data" MISE_CACHE_DIR="$DIR/mise/cache" MISE_CONFIG_DIR="$DIR/mise/config" MISE_STATE_DIR="$DIR/mise/state"
export MISE_YES=1 MISE_TRUSTED_CONFIG_PATHS="$SRC"
export CARGO_HOME="$DIR/cargo" RUSTUP_HOME="$DIR/rustup" PUB_CACHE="$DIR/pub-cache" CP_HOME_DIR="$DIR/cocoapods" GEM_HOME="$DIR/gems"
export CARGO_BUILD_JOBS="${IO_IOS_BUILD_JOBS:-4}" MAKEFLAGS="-j${IO_IOS_BUILD_JOBS:-4}"
export PATH="$DIR/bin:$GEM_HOME/bin:$PATH"

# The build runs at background priority unless told otherwise: on a machine
# somebody is working on, it must not take every core.
bg() {
  if [ -z "${IO_IOS_FULL_SPEED:-}" ] && command -v taskpolicy >/dev/null; then
    taskpolicy -b nice -n 19 "$@"
  else
    "$@"
  fi
}
sim() { xcrun simctl --set "$DEVICES" "$@"; }
step() { echo "== $*"; }

udid() { sim list devices 2>/dev/null | sed -n "s/^ *$DEVICE_NAME (\([0-9A-F-]*\)).*/\1/p" | head -1; }

build() {
  if [ -d "$APP" ] && [ "$(cat "$DIR/built-version" 2>/dev/null)" = "$APP_VERSION" ]; then
    return
  fi
  command -v xcodebuild >/dev/null || { echo "Xcode is needed" >&2; exit 1; }
  RUBY="$(brew --prefix ruby 2>/dev/null)/bin"
  [ -x "$RUBY/gem" ] || { echo "a current Ruby is needed for CocoaPods: brew install ruby" >&2; exit 1; }
  mkdir -p "$DIR/bin"

  if [ ! -x "$DIR/bin/mise" ]; then
    step "mise (the tool manager Immich's build uses)"
    tag="$(curl -fsSL https://api.github.com/repos/jdx/mise/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
    curl -fsSL -o "$DIR/bin/mise" "https://github.com/jdx/mise/releases/download/$tag/mise-$tag-macos-$( [ "$(uname -m)" = arm64 ] && echo arm64 || echo x64)"
    chmod +x "$DIR/bin/mise"
  fi
  if [ "$(git -C "$SRC" describe --tags 2>/dev/null)" != "$APP_VERSION" ]; then
    step "Immich source $APP_VERSION"
    rm -rf "$SRC"
    git clone -q --depth 1 --branch "$APP_VERSION" https://github.com/immich-app/immich.git "$SRC"
  fi
  cd "$SRC"
  step "tools (Flutter, Rust, Java, Node)"
  bg mise install node java "npm:@openapitools/openapi-generator-cli" >/dev/null
  (cd mobile && bg mise install "aqua:flutter/flutter" rust java >/dev/null)
  (cd native && bg mise install rust >/dev/null)
  if ! command -v pod >/dev/null; then
    step "CocoaPods"
    PATH="$RUBY:$PATH" bg gem install --no-document cocoapods abbrev multi_json >/dev/null
  fi
  step "the app's generated code"
  bg mise run open-api-dart >/dev/null
  cd mobile
  PATH="$RUBY:$PATH" bg mise exec -- flutter pub get >/dev/null
  PATH="$RUBY:$PATH" bg mise run codegen >/dev/null
  step "the app, for the simulator"
  PATH="$RUBY:$PATH" bg mise exec -- flutter build ios --simulator --debug | tail -3
  [ -d "$APP" ] || { echo "the build did not produce $APP" >&2; exit 1; }
  echo "$APP_VERSION" >"$DIR/built-version"
}

idb_tools() {
  [ -x "$COMPANION" ] && [ -x "$IDB" ] && return
  step "idb $IDB_VERSION"
  mkdir -p "$DIR/idb"
  base="https://github.com/facebook/idb/releases/download/$IDB_VERSION"
  curl -fsSL "$base/idb-companion.macos-arm64.tar.gz" | tar xz -C "$DIR/idb"
  curl -fsSL -o "$DIR/idb/fb_idb.whl" "$base/fb_idb-${IDB_VERSION#v}-py3-none-any.whl"
  mv "$DIR/idb/fb_idb.whl" "$DIR/idb/fb_idb-${IDB_VERSION#v}-py3-none-any.whl"
  python3 -m venv "$DIR/idb/venv"
  "$DIR/idb/venv/bin/pip" install -q "$DIR/idb/fb_idb-${IDB_VERSION#v}-py3-none-any.whl"
}

stop_helper() {
  if [ -f "$DIR/idb/companion.pid" ]; then
    kill "$(cat "$DIR/idb/companion.pid")" 2>/dev/null || true
    rm -f "$DIR/idb/companion.pid"
  fi
}

down() {
  stop_helper
  id="$(udid)"
  if [ -n "$id" ]; then
    sim shutdown "$id" >/dev/null 2>&1 || true
    sim delete "$id" >/dev/null 2>&1 || true
  fi
}

# start boots the device, installs the app with access to the photos, and
# starts the helper. The helper has to be started after the boot: it loses
# its hold on the screen when the device restarts.
start() {
  sim boot "$1"
  sim bootstatus "$1" >/dev/null
  sim install "$1" "$APP"
  sim privacy "$1" grant photos "$BUNDLE"
  nohup "$COMPANION" --udid "$1" --device-set-path "$DEVICES" --grpc-port "$PORT" >"$DIR/idb/companion.log" 2>&1 &
  echo $! >"$DIR/idb/companion.pid"
  for _ in $(seq 1 60); do
    "$IDB" --companion "localhost:$PORT" ui describe-all >/dev/null 2>&1 && return
    sleep 1
  done
  echo "the UI helper (idb) did not come up: see $DIR/idb/companion.log" >&2
  exit 1
}

reset() {
  id="$(udid)"
  [ -n "$id" ] || { echo "no device: run $0 up" >&2; exit 1; }
  stop_helper
  sim shutdown "$id" >/dev/null 2>&1 || true
  sim erase "$id"
  start "$id"
}

up() {
  build
  idb_tools
  down
  mkdir -p "$DEVICES"
  runtime="$(xcrun simctl list runtimes | sed -n 's/^iOS.* - \(com\.apple\.CoreSimulator\.SimRuntime\.iOS-[0-9-]*\).*/\1/p' | tail -1)"
  [ -n "$runtime" ] || { echo "no iOS simulator runtime: install one in Xcode (Settings, Components)" >&2; exit 1; }
  id="$(sim create "$DEVICE_NAME" "$DEVICE_TYPE" "$runtime")"
  step "booting $DEVICE_NAME ($DEVICE_TYPE, ${runtime##*.})"
  start "$id"
  echo "Ready: $id with the Immich app $APP_VERSION"
}

ready() {
  id="$(udid)"
  [ -n "$id" ] && sim list devices | grep -q "$id) (Booted)" &&
    "$IDB" --companion "localhost:$PORT" ui describe-all >/dev/null 2>&1
}

env_vars() {
  echo "E2E_IOS_SIMULATOR=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  echo "E2E_IOS_UDID=$(udid)"
  echo "E2E_IOS_DEVICES=$DEVICES"
  echo "E2E_IOS_APP=$APP"
  echo "E2E_IOS_BUNDLE=$BUNDLE"
  echo "E2E_IDB=$IDB"
  echo "E2E_IDB_COMPANION=localhost:$PORT"
}

case "${1:-}" in
  build) build && idb_tools ;;
  up) up ;;
  down) down ;;
  reset) reset ;;
  ready) ready ;;
  env) env_vars ;;
  *) echo "usage: $0 build|up|down|reset|ready|env" >&2; exit 2 ;;
esac
