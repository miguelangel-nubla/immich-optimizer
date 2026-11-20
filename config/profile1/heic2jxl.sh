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
# With "motion_photos: keep" in tasks.yaml (IO_MOTION_PHOTOS=keep) the file
# is passed through unchanged instead.
if [[ $(exiftool -b -MotionPhoto "$ORIGINAL_FILE") -gt 0 ]]; then
  if [[ "${IO_MOTION_PHOTOS:-drop_video}" == "keep" ]]; then
    echo "Original file is a HEIC Motion Photo and motion_photos is 'keep', will not optimize"
    cp "${ORIGINAL_FILE}" "${4}/${2}.${3}"
    exit 0
  fi
  echo "Original file is a HEIC Motion Photo: keeping the photo, dropping the embedded video"
fi

vips copy "${ORIGINAL_FILE}" "${TARGET_FILE}[Q=75]"
exiftool -m -overwrite_original -tagsfromfile "${ORIGINAL_FILE}" -exif "${TARGET_FILE}"
