#!/usr/bin/env bash
set -euxo pipefail

if [[ $# -lt 4 ]]; then
  echo "Usage: $0 <src_folder> <name> <extension> <dst_folder> "
  exit 1
fi

ORIGINAL_FILE="${1}/${2}.${3}"
TARGET_FILE="${4}/${2}.jxl"

# Motion photos follow the task's "motion_photos" setting, which the optimizer
# passes in IO_MOTION_PHOTOS:
#   keep        pass the file through unchanged (default)
#   drop_video  convert the photo; the embedded video is not carried over
#   drop_photo  output only the embedded video
if [[ $(exiftool -b -MotionPhoto "$ORIGINAL_FILE") -gt 0 ]]; then
  case "${IO_MOTION_PHOTOS:-keep}" in
    drop_video)
      echo "HEIC Motion Photo: keeping the photo, dropping the embedded video"
      ;;
    drop_photo)
      echo "HEIC Motion Photo: keeping the embedded video, dropping the photo"
      exiftool -b -MotionPhotoVideo "${ORIGINAL_FILE}" > "${4}/${2}.mp4"
      [[ -s "${4}/${2}.mp4" ]] || { echo "no embedded video found"; exit 1; }
      exit 0
      ;;
    *)
      echo "HEIC Motion Photo: passing it through unchanged"
      cp "${ORIGINAL_FILE}" "${4}/${2}.${3}"
      exit 0
      ;;
  esac
fi

# vips converts only the still image, and only the EXIF block is copied, so the
# motion-photo tags and the embedded video do not reach the output.
vips copy "${ORIGINAL_FILE}" "${TARGET_FILE}[Q=75]"
exiftool -m -overwrite_original -tagsfromfile "${ORIGINAL_FILE}" -exif "${TARGET_FILE}"
