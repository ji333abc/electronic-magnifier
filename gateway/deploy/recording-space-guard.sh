#!/bin/sh
set -eu

recording_dir=/mnt/recordings
minimum_free_kb=$((8 * 1024 * 1024))

mountpoint -q "$recording_dir" || exit 0

while [ "$(df -Pk "$recording_dir" | awk 'NR == 2 {print $4}')" -lt "$minimum_free_kb" ]; do
    oldest=$(find "$recording_dir" -type f -name '*.mp4' -printf '%T@ %p\n' \
        | sort -n | head -n 1 | cut -d ' ' -f 2-)
    [ -n "$oldest" ] || exit 0
    rm -f -- "$oldest"
done

find "$recording_dir" -depth -type d -empty -delete
