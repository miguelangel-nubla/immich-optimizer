#!/usr/bin/env bash
set -euxo pipefail

if [[ $# -lt 4 ]]; then
  echo "Usage: $0 <src_folder> <name> <extension> <dst_folder> "
  exit 1
fi

ORIGINAL_FILE="${1}/${2}.${3}"
TARGET_FILE="${4}/${2}.jxl"

# Motion photos: keep the photo, drop the video. vips converts only the still
# image, and only the EXIF block is copied below, so the motion-photo tags and
# the embedded video do not reach the output.
if [[ $(exiftool -b -MotionPhoto "$ORIGINAL_FILE") -gt 0 ]]; then
  echo "Original file is a HEIC Motion Photo: keeping the photo, dropping the embedded video"
fi

vips copy "${ORIGINAL_FILE}" "${TARGET_FILE}[Q=75]"
exiftool -m -overwrite_original -tagsfromfile "${ORIGINAL_FILE}" -exif "${TARGET_FILE}"
