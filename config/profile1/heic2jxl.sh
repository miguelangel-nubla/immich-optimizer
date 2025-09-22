#!/usr/bin/env bash
set -euxo pipefail

if [[ $# -lt 3 ]]; then
  echo "Usage: $0 <src_folder> <name> <extension> <dst_folder> "
  exit 1
fi

ORIGINAL_FILE="${1}/${2}.${3}"
TARGET_FILE="${4}/${2}.jxl"

if [[ $(exiftool -b -MotionPhoto "$ORIGINAL_FILE") -gt 0 ]]; then
  # TODO: Implement. https://github.com/miguelangel-nubla/immich-upload-optimizer/issues/12#issuecomment-2642944560
  echo "Original file is a HEIC Motion Photo, will not optimize"
  exit 0
else
  vips copy ${ORIGINAL_FILE} ${TARGET_FILE}[Q=75]
fi