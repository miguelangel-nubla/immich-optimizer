#!/usr/bin/env bash
set -euxo pipefail

if [[ $# -lt 3 ]]; then
  echo "Usage: $0 <folder> <name> <extension>"
  exit 1
fi

ORIGINAL_FILE_NO_EXTENSION="${1}/${2}"
ORIGINAL_FILE="${ORIGINAL_FILE_NO_EXTENSION}.${3}"

if [[ $(exiftool -b -MotionPhoto "$ORIGINAL_FILE") -gt 0 ]]; then
  # TODO: Implement. https://github.com/miguelangel-nubla/immich-upload-optimizer/issues/12#issuecomment-2642944560
  echo "Original file is a HEIC Motion Photo, will not optimize"
  exit 0
else
  vips copy ${ORIGINAL_FILE} ${ORIGINAL_FILE_NO_EXTENSION}-new.jxl[Q=75]
  rm ${ORIGINAL_FILE}
fi