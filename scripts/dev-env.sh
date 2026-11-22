# shellcheck shell=bash
# Sourced by the test scripts. It reads .dev.env from the top of the checkout,
# if there is one: a file of your own (not in git) that says where things are
# on this machine, so that it need not be typed with every command.
#
#   IO_DEV_DIR=/path/to/a/big/disk     downloads and test devices go here
#   ANDROID_HOME=/path/to/android/sdk  for the Android tests
#
# IO_DEV_DIR holds everything the tests download or create that is worth
# keeping between runs: "downloads" (the Immich app, test videos, tools) and
# the Android test device. Without it they go to the system's cache and temp
# folders.
_dev_env="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/.dev.env"
if [ -f "$_dev_env" ]; then
  set -a
  # shellcheck disable=SC1090
  . "$_dev_env"
  set +a
fi
unset _dev_env
