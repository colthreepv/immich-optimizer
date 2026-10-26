#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_NAME="${0##*/}"
readonly PRESET_ID="avif-q50-s6-420-v1"

die() {
    printf '%s: %s\n' "$SCRIPT_NAME" "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

metadata_value() {
    local file=$1
    local tag=$2
    exiftool -q -q -q -s3 -n "-$tag" "$file" 2>/dev/null || true
}

metadata_file() {
    local file=$1
    local tag=$2
    local destination=$3
    exiftool -q -q -q -b "-$tag" "$file" > "$destination" 2>/dev/null || true
}

assert_preserved() {
    local label=$1
    local before=$2
    local after=$3
    if [[ -n $before && $before != "$after" ]]; then
        die "metadata validation failed for $label (before='$before', after='$after')"
    fi
}

expected_irot() {
    case "$1" in
        3) printf '2\n' ;;
        5|8) printf '1\n' ;;
        6|7) printf '3\n' ;;
        *) printf '\n' ;;
    esac
}

expected_imir() {
    case "$1" in
        2) printf '1\n' ;;
        4|5|7) printf '0\n' ;;
        *) printf '\n' ;;
    esac
}

[[ $# -eq 2 ]] || die "usage: $SCRIPT_NAME INPUT_JPEG OUTPUT_AVIF"

input=$1
output=$2
[[ -f $input && -r $input ]] || die "input JPEG is not readable: $input"
[[ $output == *.avif ]] || die "output must use the .avif extension: $output"
[[ $(realpath -m "$input") != "$(realpath -m "$output")" ]] || die "input and output paths must differ"

require_command exiftool
require_command avifenc
require_command avifdec
require_command quiet-exec
require_command realpath
require_command sha256sum
require_command setsid

jobs=${AVIF_JOBS:-1}
[[ $jobs =~ ^[1-9][0-9]*$ ]] || die "AVIF_JOBS must be a positive integer"

output_dir=$(dirname "$output")
[[ -d $output_dir && -w $output_dir ]] || die "output directory is not writable: $output_dir"

work_dir=$(mktemp -d "$output_dir/.avif-q50-s6-420-v1.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

[[ $(metadata_value "$input" FileType) == "JPEG" ]] || die "input is not a JPEG: $input"

# Reject JPEG containers with payloads that this processor cannot carry into
# AVIF. Failing here keeps the original source instead of dropping features.
if [[ -n $(metadata_value "$input" MPImage2) || -n $(metadata_value "$input" MPImageCount) ]]; then
    die "unsupported multi-picture JPEG (MPF)"
fi
metadata_file "$input" XMP "$work_dir/source.xmp"
if grep -aEq 'GCamera:MicroVideo|GCamera:MotionPhoto|Camera:MotionPhoto|hdrgm:|urn:com:apple:photo:2020:aux:hdrgainmap' "$work_dir/source.xmp"; then
    die "unsupported motion or HDR JPEG"
fi

source_width=$(metadata_value "$input" ImageWidth)
source_height=$(metadata_value "$input" ImageHeight)
source_date=$(metadata_value "$input" DateTimeOriginal)
source_offset=$(metadata_value "$input" OffsetTimeOriginal)
source_latitude=$(metadata_value "$input" GPSLatitude)
source_longitude=$(metadata_value "$input" GPSLongitude)
source_orientation=$(metadata_value "$input" Orientation)

[[ $source_width =~ ^[1-9][0-9]*$ && $source_height =~ ^[1-9][0-9]*$ ]] || die "unable to determine JPEG dimensions"
[[ -z $source_orientation || $source_orientation =~ ^[1-8]$ ]] || die "unsupported EXIF orientation value: $source_orientation"

metadata_file "$input" ICC_Profile "$work_dir/source.icc"
source_icc_sha=""
if [[ -s $work_dir/source.icc ]]; then
    source_icc_sha=$(sha256sum "$work_dir/source.icc" | awk '{print $1}')
fi

temporary_avif="$work_dir/$PRESET_ID.avif"
quiet-exec avifenc \
    --qcolor 50 \
    --jobs "$jobs" \
    --speed 6 \
    --yuv 420 \
    "$input" \
    "$temporary_avif"

[[ -s $temporary_avif ]] || die "avifenc did not produce an output file"

avif_info=$(avifdec -i "$temporary_avif")
resolution=$(awk -F': ' '/^ \* Resolution[[:space:]]*:/ {gsub(/^[[:space:]]+|[[:space:]]+$/, "", $2); print $2; exit}' <<< "$avif_info")
[[ $resolution == "${source_width}x${source_height}" ]] || die "dimension validation failed (JPEG ${source_width}x${source_height}, AVIF '${resolution}')"

grep -Eq '^ \* Format[[:space:]]*: YUV420$' <<< "$avif_info" || die "AVIF output is not 4:2:0"
grep -Eq '^ \* Color Primaries[[:space:]]*:' <<< "$avif_info" || die "AVIF output has no color primaries signal"
grep -Eq '^ \* Transfer Char\.[[:space:]]*:' <<< "$avif_info" || die "AVIF output has no transfer characteristics signal"
grep -Eq '^ \* Matrix Coeffs\.[[:space:]]*:' <<< "$avif_info" || die "AVIF output has no matrix coefficients signal"

expected_rotation=$(expected_irot "$source_orientation")
expected_mirror=$(expected_imir "$source_orientation")
if [[ -n $expected_rotation ]]; then
    grep -Eq "^[[:space:]]*\* irot \\(Rotation\\)[[:space:]]*: ${expected_rotation}$" <<< "$avif_info" || die "EXIF orientation was not represented as the expected irot transform"
fi
if [[ -n $expected_mirror ]]; then
    grep -Eq "^[[:space:]]*\* imir \\(Mirror\\)[[:space:]]*: ${expected_mirror} " <<< "$avif_info" || die "EXIF orientation was not represented as the expected imir transform"
fi

assert_preserved "DateTimeOriginal" "$source_date" "$(metadata_value "$temporary_avif" DateTimeOriginal)"
assert_preserved "OffsetTimeOriginal" "$source_offset" "$(metadata_value "$temporary_avif" OffsetTimeOriginal)"
assert_preserved "GPSLatitude" "$source_latitude" "$(metadata_value "$temporary_avif" GPSLatitude)"
assert_preserved "GPSLongitude" "$source_longitude" "$(metadata_value "$temporary_avif" GPSLongitude)"

if [[ -n $source_icc_sha ]]; then
    metadata_file "$temporary_avif" ICC_Profile "$work_dir/output.icc"
    [[ -s $work_dir/output.icc ]] || die "AVIF output lost the source ICC profile"
    output_icc_sha=$(sha256sum "$work_dir/output.icc" | awk '{print $1}')
    [[ $source_icc_sha == "$output_icc_sha" ]] || die "AVIF output changed the source ICC profile"
fi

mv -f -- "$temporary_avif" "$output"
