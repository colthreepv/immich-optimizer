#!/usr/bin/env bash
set -Eeuo pipefail

readonly SCRIPT_NAME="${0##*/}"
readonly X265_CODEC=libx265
readonly AV1_CODEC=libsvtav1

die() {
    printf '%s: %s\n' "$SCRIPT_NAME" "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

is_uint() {
    [[ ${1:-} =~ ^[0-9]+$ ]]
}

is_number() {
    [[ ${1:-} =~ ^([0-9]+([.][0-9]+)?|[.][0-9]+)$ ]]
}

probe_summary() {
    local file=$1

    "$ffprobe_bin" -v error \
        -show_entries 'format=duration:format_tags:stream=index,codec_type,codec_name,width,height,pix_fmt,color_transfer,color_primaries,color_range:stream_tags:stream_disposition=default,forced,attached_pic:stream_side_data' \
        -of json "$file" |
        "$perl_bin" -MJSON::PP -e '
            local $/;
            my $input = <STDIN>;
            my $root = eval { decode_json($input) };
            die "invalid ffprobe JSON\n" unless $root;

            my $format = $root->{format} // {};
            print "duration\t", ($format->{duration} // ""), "\n";
            my $format_tags = $format->{tags} // {};
            for my $key (keys %$format_tags) {
                my $value = $format_tags->{$key} // "";
                $value =~ s/[\r\n]+/ /g;
                $value =~ s/\s+$//;
                print "ftag\t", lc($key), "\t$value\n";
            }

            for my $stream (@{$root->{streams} // []}) {
                my $disposition = $stream->{disposition} // {};
                my $tags = $stream->{tags} // {};
                my $language = lc($tags->{language} // "");
                my $title = $tags->{title} // "";
                $title =~ s/[\t\r\n]+/ /g;
                my $rotation = 0;
                for my $side_data (@{$stream->{side_data_list} // []}) {
                    $rotation = $side_data->{rotation} if defined $side_data->{rotation};
                }

                print join("\t",
                    "stream",
                    $stream->{index} // "",
                    $stream->{codec_type} // "",
                    $stream->{codec_name} // "",
                    $stream->{width} // "",
                    $stream->{height} // "",
                    $rotation,
                    $stream->{color_transfer} // "",
                    $stream->{color_primaries} // "",
                    $stream->{color_range} // "",
                    $disposition->{default} // 0,
                    $disposition->{forced} // 0,
                    $disposition->{attached_pic} // 0,
                    $language,
                    $title,
                ), "\n";

                for my $key (keys %$tags) {
                    my $value = $tags->{$key} // "";
                    $value =~ s/[\t\r\n]+/ /g;
                    print "stag\t", ($stream->{index} // ""), "\t", lc($key), "\t$value\n";
                }
                for my $side_data (@{$stream->{side_data_list} // []}) {
                    print "sidedata\t", ($stream->{index} // ""), "\t",
                        ($side_data->{side_data_type} // ""), "\n";
                }
            }
        '
}

stream_field() {
    local file=$1
    local field=$2

    awk -F'\t' -v field="$field" '
        $1 == "stream" && $3 == "video" { print $field; exit }
    ' "$file"
}

format_tag() {
    local file=$1
    local key=$2

    awk -F'\t' -v key="$key" '
        $1 == "ftag" && $2 == key {
            sub(/^[^\t]*\t[^\t]*\t/, "")
            print
            exit
        }
    ' "$file"
}

location_value() {
    local file=$1

    awk -F'\t' '
        ($1 == "ftag" &&
            ($2 == "location" || $2 == "location-eng" ||
             $2 == "com.apple.quicktime.location.iso6709")) ||
        ($1 == "stag" &&
            ($3 == "location" || $3 == "location-eng" ||
             $3 == "com.apple.quicktime.location.iso6709")) {
            if ($1 == "ftag") {
                sub(/^[^\t]*\t[^\t]*\t/, "")
            } else {
                sub(/^[^\t]*\t[^\t]*\t[^\t]*\t/, "")
            }
            print
            exit
        }
    ' "$file"
}

location_values() {
    local file=$1

    awk -F'\t' '
        ($1 == "ftag" && ($2 ~ /location/ || $2 ~ /iso6709/)) ||
        ($1 == "stag" && ($3 ~ /location/ || $3 ~ /iso6709/)) {
            if ($1 == "ftag") {
                sub(/^[^\t]*\t[^\t]*\t/, "")
            } else {
                sub(/^[^\t]*\t[^\t]*\t[^\t]*\t/, "")
            }
            print
        }
    ' "$file"
}

video_stream_count() {
    local file=$1

    awk -F'\t' '$1 == "stream" && $3 == "video" { count += 1 } END { print count + 0 }' "$file"
}

non_video_signature() {
    local file=$1

    awk -F'\t' '
        $1 == "stream" && $3 != "video" {
            printf "%s|%s|%s|%s|%s\n", $3, $4, $14, $11, $12
        }
    ' "$file"
}

assert_real_video() {
    local probe=$1
    local label=$2
    local count duration

    count=$(video_stream_count "$probe")
    [[ $count -eq 1 ]] || die "$label must contain exactly one video stream (found $count)"

    duration=$(awk -F'\t' '$1 == "duration" { print $2; exit }' "$probe")
    is_number "$duration" || die "$label has no usable duration"
    awk -v duration="$duration" 'BEGIN { exit !(duration > 0) }' ||
        die "$label duration must be greater than zero"
}

reject_unsupported_features() {
    local probe=$1

    if ! awk -F'\t' '
        $1 == "stream" && $3 == "video" {
            transfer = tolower($8)
            primaries = tolower($9)
            if (transfer == "smpte2084" || transfer == "arib-std-b67" || primaries == "bt2020") {
                exit 10
            }
        }
        $1 == "sidedata" {
            side_data = tolower($3)
            if (side_data ~ /dovi|hdr|mastering|content light|spherical|stereo|3d|projection|multiview/) {
                exit 11
            }
        }
        $1 == "ftag" {
            key = tolower($2)
            value = tolower($3)
            if (key ~ /motionphoto|microvideo|hdr|dovi|stereoscopic|spatial/ ||
                value ~ /motionphoto|microvideo|dovi/) {
                exit 12
            }
        }
        $1 == "stag" {
            key = tolower($3)
            value = tolower($4)
            if (key ~ /motionphoto|microvideo|hdr|dovi|stereoscopic|spatial/ ||
                value ~ /motionphoto|microvideo|dovi/) {
                exit 13
            }
        }
    ' "$probe"; then
        die "unsupported HDR, motion, spatial or dynamic-metadata video"
    fi
}

validate_candidate() {
    local input_probe=$1
    local candidate_probe=$2
    local expected_codec=$3
    local source_duration candidate_duration source_creation candidate_creation
    local source_location candidate_location source_rotation candidate_rotation
    local source_width source_height candidate_width candidate_height

    assert_real_video "$candidate_probe" "candidate"
    [[ $(stream_field "$candidate_probe" 4) == "$expected_codec" ]] ||
        die "candidate codec is '$(stream_field "$candidate_probe" 4)', expected '$expected_codec'"

    source_width=$(stream_field "$input_probe" 5)
    source_height=$(stream_field "$input_probe" 6)
    candidate_width=$(stream_field "$candidate_probe" 5)
    candidate_height=$(stream_field "$candidate_probe" 6)
    [[ $source_width == "$candidate_width" && $source_height == "$candidate_height" ]] ||
        die "resolution validation failed (source ${source_width}x${source_height}, candidate ${candidate_width}x${candidate_height})"

    source_rotation=$(stream_field "$input_probe" 7)
    candidate_rotation=$(stream_field "$candidate_probe" 7)
    [[ $source_rotation == "$candidate_rotation" ]] ||
        die "rotation validation failed (source '$source_rotation', candidate '$candidate_rotation')"

    source_duration=$(awk -F'\t' '$1 == "duration" { print $2; exit }' "$input_probe")
    candidate_duration=$(awk -F'\t' '$1 == "duration" { print $2; exit }' "$candidate_probe")
    awk -v source="$source_duration" -v candidate="$candidate_duration" -v tolerance="$duration_tolerance" '
        BEGIN {
            delta = source - candidate
            if (delta < 0) delta = -delta
            exit !(delta <= tolerance)
        }
    ' || die "duration validation failed (source ${source_duration}s, candidate ${candidate_duration}s, tolerance ${duration_tolerance}s)"

    source_creation=$(format_tag "$input_probe" creation_time)
    candidate_creation=$(format_tag "$candidate_probe" creation_time)
    if [[ -n $source_creation && $source_creation != "$candidate_creation" ]]; then
        die "creation_time validation failed (source '$source_creation', candidate '$candidate_creation')"
    fi

    source_location=$(location_value "$input_probe")
    if [[ -n $source_location ]]; then
        candidate_location=$(location_values "$candidate_probe")
        grep -Fqx -- "$source_location" <<< "$candidate_location" ||
            die "location validation failed (source '$source_location' not found in candidate metadata)"
    fi

    [[ $(non_video_signature "$input_probe") == "$(non_video_signature "$candidate_probe")" ]] ||
        die "audio, subtitle or auxiliary stream validation failed"

    reject_unsupported_features "$candidate_probe"
}

acquire_slot() {
    local index fd

    [[ -n $lock_dir ]] || die "video concurrency lock directory is empty"
    mkdir -p -- "$lock_dir" || die "unable to create video lock directory: $lock_dir"

    while :; do
        for ((index = 0; index < video_concurrency; index += 1)); do
            exec {fd}>"$lock_dir/slot-$index"
            if flock -n "$fd"; then
                slot_fd=$fd
                return 0
            fi
            exec {fd}>&-
        done
        sleep 1 || true
    done
}

[[ $# -eq 2 ]] || die "usage: $SCRIPT_NAME INPUT_VIDEO OUTPUT_PREFIX"

input=$1
output_prefix=$2
[[ -f $input && -r $input ]] || die "input video is not readable: $input"
[[ -n $output_prefix ]] || die "output prefix must not be empty"

enabled_raw=${VIDEO_TRANSCODE_ENABLED:-0}
case "${enabled_raw,,}" in
    0|false|no|off) enabled=0 ;;
    1|true|yes|on) enabled=1 ;;
    *) die "VIDEO_TRANSCODE_ENABLED must be 0/1, true/false, yes/no, or on/off" ;;
esac

ffmpeg_bin=${FFMPEG_BIN:-ffmpeg}
ffprobe_bin=${FFPROBE_BIN:-ffprobe}
quiet_exec_bin=${QUIET_EXEC_BIN:-quiet-exec}
perl_bin=${PERL_BIN:-perl}

require_command "$ffprobe_bin"
require_command "$perl_bin"
require_command realpath
require_command mktemp
require_command cp
require_command mv

output_parent=$(dirname -- "$output_prefix")
[[ -d $output_parent && -w $output_parent ]] || die "output directory is not writable: $output_parent"

work_dir=$(mktemp -d "$output_parent/.video-candidate.XXXXXX")
trap 'rm -rf -- "$work_dir"' EXIT

input_probe="$work_dir/input.probe"
probe_summary "$input" > "$input_probe" || die "unable to probe input video: $input"
assert_real_video "$input_probe" "input"

if (( ! enabled )); then
    extension=${input##*.}
    [[ $extension != "$input" && -n $extension ]] || die "input video has no extension: $input"

    final_candidate="$output_prefix.$extension"
    [[ $(realpath -m -- "$input") != "$(realpath -m -- "$final_candidate")" ]] ||
        die "candidate path must differ from the source path"
    [[ ! -e $final_candidate ]] || die "candidate already exists: $final_candidate"

    cp -p -- "$input" "$work_dir/candidate.$extension" || die "unable to copy the disabled-pipeline video"
    mv -- "$work_dir/candidate.$extension" "$final_candidate"
    printf '%s: video transcoding disabled; copied original bytes to %s; source retained\n' \
        "$SCRIPT_NAME" "$final_candidate" >&2
    exit 0
fi

video_codec=${VIDEO_CODEC:-}
video_preset=${VIDEO_PRESET:-}
video_crf=${VIDEO_CRF:-}
video_bitrate=${VIDEO_BITRATE:-}
video_threads=${VIDEO_THREADS:-1}
video_concurrency=${VIDEO_CONCURRENCY:-1}
duration_tolerance=${VIDEO_DURATION_TOLERANCE_SECONDS:-0.5}
lock_dir=${VIDEO_LOCK_DIR:-${TMPDIR:-/tmp}/immich-optimizer-video-slots}

[[ -n $video_codec ]] || die "VIDEO_CODEC is required when VIDEO_TRANSCODE_ENABLED is on"
[[ -n $video_preset ]] || die "VIDEO_PRESET is required when VIDEO_TRANSCODE_ENABLED is on"
[[ -n $video_crf || -n $video_bitrate ]] ||
    die "VIDEO_CRF or VIDEO_BITRATE is required when VIDEO_TRANSCODE_ENABLED is on"
[[ -z $video_crf || -z $video_bitrate ]] ||
    die "set only one of VIDEO_CRF and VIDEO_BITRATE"

is_uint "$video_threads" && (( video_threads > 0 && video_threads <= 64 )) ||
    die "VIDEO_THREADS must be an integer from 1 to 64"
is_uint "$video_concurrency" && (( video_concurrency > 0 && video_concurrency <= 16 )) ||
    die "VIDEO_CONCURRENCY must be an integer from 1 to 16"
is_number "$duration_tolerance" ||
    die "VIDEO_DURATION_TOLERANCE_SECONDS must be a non-negative number"

case "$video_codec" in
    "$X265_CODEC")
        extension=mp4
        expected_codec=hevc
        [[ $video_preset =~ ^(ultrafast|superfast|veryfast|faster|fast|medium|slow|slower|veryslow|placebo)$ ]] ||
            die "invalid x265 VIDEO_PRESET: $video_preset"
        if [[ -n $video_crf ]]; then
            is_uint "$video_crf" && (( video_crf >= 0 && video_crf <= 51 )) ||
                die "x265 VIDEO_CRF must be an integer from 0 to 51"
        fi
        ;;
    "$AV1_CODEC")
        extension=mkv
        expected_codec=av1
        [[ $video_preset =~ ^([0-9]|1[0-3])$ ]] ||
            die "libsvtav1 VIDEO_PRESET must be an integer from 0 to 13"
        if [[ -n $video_crf ]]; then
            is_uint "$video_crf" && (( video_crf >= 0 && video_crf <= 63 )) ||
                die "libsvtav1 VIDEO_CRF must be an integer from 0 to 63"
        fi
        ;;
    *) die "VIDEO_CODEC must be $X265_CODEC or $AV1_CODEC" ;;
esac

if [[ -n $video_bitrate ]]; then
    [[ $video_bitrate =~ ^[1-9][0-9]*([kKmM])?$ ]] ||
        die "VIDEO_BITRATE must be a positive value such as 4000k or 4M"
fi

require_command "$ffmpeg_bin"
require_command "$quiet_exec_bin"
require_command flock
require_command setsid

final_candidate="$output_prefix.$extension"
[[ $(realpath -m -- "$input") != "$(realpath -m -- "$final_candidate")" ]] ||
    die "candidate path must differ from the source path"
[[ ! -e $final_candidate ]] || die "candidate already exists: $final_candidate"

reject_unsupported_features "$input_probe"
acquire_slot

quality_args=()
if [[ -n $video_crf ]]; then
    quality_args=(-crf "$video_crf")
else
    quality_args=(-b:v "$video_bitrate")
fi

ffmpeg_args=(
    -nostdin
    -hide_banner
    -loglevel error
    -noautorotate
    -i "$input"
    -map 0
    -map_chapters 0
    -map_metadata 0
    -c copy
    -c:v:0 "$video_codec"
    "${quality_args[@]}"
    -preset "$video_preset"
    -threads "$video_threads"
)

if [[ $video_codec == "$AV1_CODEC" ]]; then
    ffmpeg_args+=(-svtav1-params "lp=$video_threads")
else
    ffmpeg_args+=(
        -x265-params "pools=$video_threads:frame-threads=$video_threads"
        -tag:v:0 hvc1
        -movflags +faststart
    )
fi

temporary_candidate="$work_dir/candidate.$extension"
ffmpeg_args+=(-y "$temporary_candidate")

"$quiet_exec_bin" "$ffmpeg_bin" "${ffmpeg_args[@]}" ||
    die "ffmpeg failed; no candidate was published"
[[ -s $temporary_candidate ]] || die "ffmpeg produced no candidate"

candidate_probe="$work_dir/candidate.probe"
probe_summary "$temporary_candidate" > "$candidate_probe" ||
    die "unable to probe candidate"
validate_candidate "$input_probe" "$candidate_probe" "$expected_codec"

input_size=$(wc -c < "$input")
candidate_size=$(wc -c < "$temporary_candidate")
if (( candidate_size >= input_size )); then
    printf '%s: candidate is not smaller (%s >= %s bytes); the pipeline will retain the original\n' \
        "$SCRIPT_NAME" "$candidate_size" "$input_size" >&2
fi

mv -- "$temporary_candidate" "$final_candidate"
printf '%s: candidate ready at %s (%s -> %s bytes)\n' \
    "$SCRIPT_NAME" "$final_candidate" "$input_size" "$candidate_size" >&2
