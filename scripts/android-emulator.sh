#!/usr/bin/env bash
# Brings an Android emulator with the Immich app up or down, for the phone
# end-to-end tests (E2E_ANDROID=1 scripts/e2e-test.sh).
#
#   scripts/android-emulator.sh up       create the device, boot it, install the app
#   scripts/android-emulator.sh down     stop it and delete the device
#   scripts/android-emulator.sh serial   print the adb serial of the device
#
# Needs an Android SDK (ANDROID_HOME) with the emulator and one system image:
#   sdkmanager "emulator" "platform-tools" "system-images;android-36;google_apis;<arch>"
# where <arch> is arm64-v8a on Apple silicon and x86_64 elsewhere. The
# "google_apis" image (not "playstore") is needed: the tests read the app's
# database, which takes root.
#
# Nothing is kept: the device lives in a temporary folder (IO_ANDROID_DIR) and
# "down" removes it.
set -euo pipefail

: "${ANDROID_HOME:?set ANDROID_HOME to your Android SDK}"
API="${IO_ANDROID_API:-36}"
APP_VERSION="${IMMICH_APP_VERSION:-v3.4.2}"
DIR="${IO_ANDROID_DIR:-${TMPDIR:-/tmp}/io-android}"
PORT="${IO_ANDROID_PORT:-5580}"
# Processor cores the device gets (IO_ANDROID_CORES). Fewer keeps the machine
# it runs on usable; the tests only get slower.
CORES="${IO_ANDROID_CORES:-4}"
SERIAL="emulator-$PORT"
AVD="io-e2e"
ADB="$ANDROID_HOME/platform-tools/adb"
EMULATOR="$ANDROID_HOME/emulator/emulator"

case "$(uname -m)" in
  arm64 | aarch64) ABI=arm64-v8a ;;
  *) ABI=x86_64 ;;
esac
IMAGE="system-images/android-$API/google_apis/$ABI"

down() {
  "$ADB" -s "$SERIAL" emu kill >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do
    "$ADB" devices | grep -q "^$SERIAL" || break
    sleep 1
  done
  rm -rf "$DIR"
}

up() {
  [ -x "$EMULATOR" ] || { echo "no emulator in $ANDROID_HOME: install it with sdkmanager \"emulator\"" >&2; exit 1; }
  [ -d "$ANDROID_HOME/$IMAGE" ] || {
    echo "no system image: install it with sdkmanager \"system-images;android-$API;google_apis;$ABI\"" >&2
    exit 1
  }
  down
  mkdir -p "$DIR/avd/$AVD.avd" "$DIR/cache"
  export ANDROID_AVD_HOME="$DIR/avd"
  cat >"$DIR/avd/$AVD.ini" <<INI
avd.ini.encoding=UTF-8
path=$DIR/avd/$AVD.avd
target=android-$API
INI
  cat >"$DIR/avd/$AVD.avd/config.ini" <<INI
AvdId=$AVD
abi.type=$ABI
avd.ini.encoding=UTF-8
disk.dataPartition.size=6G
hw.cpu.arch=$([ "$ABI" = arm64-v8a ] && echo arm64 || echo x86_64)
hw.cpu.ncore=$CORES
hw.gpu.enabled=yes
hw.gpu.mode=swiftshader_indirect
hw.keyboard=yes
hw.lcd.density=420
hw.lcd.height=2400
hw.lcd.width=1080
hw.ramSize=3072
hw.sdCard=yes
image.sysdir.1=$IMAGE/
sdcard.size=512M
tag.display=Google APIs
tag.id=google_apis
INI

  nohup "$EMULATOR" -avd "$AVD" -port "$PORT" -no-window -no-audio -no-boot-anim -no-snapshot \
    -gpu swiftshader_indirect >"$DIR/emulator.log" 2>&1 &

  echo "Booting $SERIAL (Android $API, $ABI)..."
  "$ADB" -s "$SERIAL" wait-for-device
  for _ in $(seq 1 180); do
    [ "$("$ADB" -s "$SERIAL" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = 1 ] && break
    sleep 2
  done
  [ "$("$ADB" -s "$SERIAL" shell getprop sys.boot_completed | tr -d '\r')" = 1 ] || {
    echo "the emulator did not finish booting; see $DIR/emulator.log" >&2
    exit 1
  }
  # Right after boot adbd can drop the connection while it restarts as root.
  for _ in $(seq 1 30); do
    "$ADB" -s "$SERIAL" root >/dev/null 2>&1 || true
    "$ADB" -s "$SERIAL" wait-for-device
    [ "$("$ADB" -s "$SERIAL" shell id -u 2>/dev/null | tr -d '\r')" = 0 ] && break
    sleep 2
  done
  [ "$("$ADB" -s "$SERIAL" shell id -u | tr -d '\r')" = 0 ] || { echo "could not get root on the emulator" >&2; exit 1; }
  # No animations: screens settle at once, which keeps scripted taps reliable.
  for k in window_animation_scale transition_animation_scale animator_duration_scale; do
    "$ADB" -s "$SERIAL" shell settings put global "$k" 0
  done
  "$ADB" -s "$SERIAL" shell settings put system screen_off_timeout 2147483647
  # No on-screen keyboard (the device has a hardware one): it slides in while
  # the first characters are typed, and the app then loses some of them.
  "$ADB" -s "$SERIAL" shell settings put secure show_ime_with_hard_keyboard 0

  APK="${IO_ANDROID_APK_CACHE:-$HOME/.cache/immich-optimizer}/immich-$APP_VERSION-$ABI.apk"
  if [ ! -s "$APK" ]; then
    mkdir -p "$(dirname "$APK")"
    echo "Downloading the Immich app $APP_VERSION..."
    curl -fsSL -o "$APK.part" "https://github.com/immich-app/immich/releases/download/$APP_VERSION/app-$ABI-release.apk"
    mv "$APK.part" "$APK"
  fi
  "$ADB" -s "$SERIAL" install -r -g "$APK" >/dev/null
  echo "Ready: $SERIAL with the Immich app $APP_VERSION"
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  serial) echo "$SERIAL" ;;
  *) echo "usage: $0 up|down|serial" >&2; exit 2 ;;
esac
