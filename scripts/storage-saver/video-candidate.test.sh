#!/usr/bin/env bash
set -Eeuo pipefail

test_dir=$(mktemp -d)
cleanup() {
    rm -rf -- "$test_dir"
}
trap cleanup EXIT

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
candidate_source=${VIDEO_CANDIDATE_BIN:-$script_dir/video-candidate.sh}
quiet_exec_source=${QUIET_EXEC_BIN:-$script_dir/quiet-exec}
ffmpeg=${FFMPEG_BIN:-ffmpeg}
ffprobe=${FFPROBE_BIN:-ffprobe}

bin_dir="$test_dir/bin"
mkdir -p "$bin_dir"

install_executable() {
    local source=$1
    local name=$2

    cp -- "$source" "$bin_dir/$name"
    chmod 0755 "$bin_dir/$name"
    printf '%s\n' "$bin_dir/$name"
}

candidate=$(install_executable "$candidate_source" video-candidate.sh)
quiet_exec=$(install_executable "$quiet_exec_source" quiet-exec)

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

assert_file() {
    [[ -f $1 ]] || fail "expected file is missing: $1"
}

assert_no_file() {
    [[ ! -e $1 ]] || fail "unexpected file exists: $1"
}

assert_equal() {
    [[ $1 == "$2" ]] || fail "$3 (expected '$2', got '$1')"
}

sha256() {
    sha256sum "$1" | awk '{ print $1 }'
}

probe_value() {
    local file=$1
    local select_streams=$2
    local entries=$3

    if [[ -n $select_streams ]]; then
        "$ffprobe" -v error -select_streams "$select_streams" \
            -show_entries "$entries" -of default=nw=1:nk=1 "$file" | head -n 1
    else
        "$ffprobe" -v error -show_entries "$entries" \
            -of default=nw=1:nk=1 "$file" | head -n 1
    fi
}

stream_count() {
    local file=$1
    local kind=$2

    "$ffprobe" -v error -select_streams "$kind" -show_entries stream=index \
        -of csv=p=0 "$file" | awk 'NF { count += 1 } END { print count + 0 }'
}

assert_runs() {
    if ! "$@"; then
        fail "command failed: $*"
    fi
}

expect_failure() {
    local label=$1
    local output_prefix=$2
    shift 2

    if "$@" >"$test_dir/$label.log" 2>&1; then
        fail "$label unexpectedly succeeded"
    fi
    compgen -G "$output_prefix.*" >/dev/null && fail "$label published a candidate"
    return 0
}

mkdir -p "$test_dir/locks"

cat > "$test_dir/subtitle.ass" <<'EOF'
[Script Info]
ScriptType: v4.00+
[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1
[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:00.00,0:00:01.00,Default,,0,0,0,,video candidate test
EOF

"$ffmpeg" -hide_banner -loglevel error \
    -f lavfi -i 'testsrc2=size=320x240:rate=25:duration=2' \
    -f lavfi -i 'sine=frequency=440:duration=2' \
    -map 0:v:0 -map 1:a:0 \
    -c:v mpeg4 -q:v 2 -c:a aac -b:a 64k \
    -metadata creation_time='2024-05-01T10:00:00Z' \
    -metadata location='+13.7563+100.5018/' \
    -y "$test_dir/base.mp4"

"$ffmpeg" -hide_banner -loglevel error \
    -display_rotation 90 -i "$test_dir/base.mp4" \
    -map 0 -c copy -map_metadata 0 -y "$test_dir/source-rotated.mp4"

"$ffmpeg" -hide_banner -loglevel error \
    -i "$test_dir/source-rotated.mp4" -map 0 -c copy -f mov \
    -y "$test_dir/source-disabled.mov"

"$ffmpeg" -hide_banner -loglevel error \
    -f lavfi -i 'testsrc2=size=160x120:rate=15:duration=1' \
    -f lavfi -i 'sine=frequency=660:duration=1' \
    -i "$test_dir/subtitle.ass" \
    -map 0:v:0 -map 1:a:0 -map 2:s:0 \
    -c:v mpeg4 -q:v 3 -c:a aac -b:a 48k -c:s ass \
    -metadata creation_time='2024-06-02T11:00:00Z' \
    -metadata location='+13.7000+100.6000/' \
    -y "$test_dir/source-subtitles.mkv"

"$ffmpeg" -hide_banner -loglevel error \
    -f lavfi -i 'testsrc2=size=160x120:rate=10:duration=1' \
    -c:v libx265 -preset ultrafast \
    -x265-params 'log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc' \
    -crf 30 \
    -color_trc smpte2084 -color_primaries bt2020 \
    -y "$test_dir/source-hdr.mp4"

[[ $(probe_value "$test_dir/source-rotated.mp4" v:0 stream_side_data=rotation) == 90 ]] ||
    fail "synthetic source rotation was not created"
[[ $(probe_value "$test_dir/source-subtitles.mkv" s stream=codec_name) == ass ]] ||
    fail "synthetic subtitle stream was not created"
[[ $(stream_count "$test_dir/source-subtitles.mkv" a) -eq 1 ]] ||
    fail "synthetic audio stream was not created"

disabled_source="$test_dir/source-disabled.mov"
disabled_hash=$(sha256 "$disabled_source")
fake_ffmpeg="$test_dir/fake-ffmpeg"
cat > "$fake_ffmpeg" <<'EOF'
#!/usr/bin/env bash
printf 'called\n' >> "$FAKE_FFMPEG_MARKER"
exit 99
EOF
chmod +x "$fake_ffmpeg"
export FAKE_FFMPEG_MARKER="$test_dir/fake-ffmpeg-called"

VIDEO_TRANSCODE_ENABLED=0 \
VIDEO_LOCK_DIR="$test_dir/locks" \
FFMPEG_BIN="$fake_ffmpeg" \
FFPROBE_BIN="$ffprobe" \
    "$candidate" "$disabled_source" "$test_dir/disabled-out" \
    2>"$test_dir/disabled.log"
assert_file "$test_dir/disabled-out.mov"
assert_no_file "$test_dir/disabled-out.mp4"
assert_no_file "$test_dir/fake-ffmpeg-called"
cmp -s "$disabled_source" "$test_dir/disabled-out.mov" || fail "disabled copy is not byte-identical"
assert_equal "$(sha256 "$disabled_source")" "$disabled_hash" "disabled source changed"

expect_failure missing-codec "$test_dir/missing-codec-out" \
    env VIDEO_TRANSCODE_ENABLED=1 VIDEO_LOCK_DIR="$test_dir/locks" \
        "$candidate" "$test_dir/source-rotated.mp4" "$test_dir/missing-codec-out"
expect_failure missing-quality "$test_dir/missing-quality-out" \
    env VIDEO_TRANSCODE_ENABLED=1 VIDEO_CODEC=libx265 VIDEO_PRESET=ultrafast \
        VIDEO_LOCK_DIR="$test_dir/locks" \
        "$candidate" "$test_dir/source-rotated.mp4" "$test_dir/missing-quality-out"
expect_failure missing-preset "$test_dir/missing-preset-out" \
    env VIDEO_TRANSCODE_ENABLED=1 VIDEO_CODEC=libx265 VIDEO_CRF=30 \
        VIDEO_LOCK_DIR="$test_dir/locks" \
        "$candidate" "$test_dir/source-rotated.mp4" "$test_dir/missing-preset-out"
expect_failure ambiguous-quality "$test_dir/ambiguous-out" \
    env VIDEO_TRANSCODE_ENABLED=1 VIDEO_CODEC=libx265 VIDEO_PRESET=ultrafast \
        VIDEO_CRF=30 VIDEO_BITRATE=2M VIDEO_LOCK_DIR="$test_dir/locks" \
        "$candidate" "$test_dir/source-rotated.mp4" "$test_dir/ambiguous-out"

x265_source="$test_dir/source-rotated.mp4"
x265_hash=$(sha256 "$x265_source")
VIDEO_TRANSCODE_ENABLED=1 \
VIDEO_CODEC=libx265 \
VIDEO_PRESET=ultrafast \
VIDEO_CRF=30 \
VIDEO_THREADS=1 \
VIDEO_CONCURRENCY=1 \
VIDEO_LOCK_DIR="$test_dir/locks" \
QUIET_EXEC_BIN="$quiet_exec" \
QUIET_EXEC_SENSOR_ENABLED=0 \
    "$candidate" "$x265_source" "$test_dir/x265-out" \
    2>"$test_dir/x265.log"
assert_file "$test_dir/x265-out.mp4"
assert_equal "$(sha256 "$x265_source")" "$x265_hash" "x265 source changed"
assert_equal "$(probe_value "$test_dir/x265-out.mp4" v:0 stream=codec_name)" hevc "x265 candidate codec"
assert_equal "$(probe_value "$test_dir/x265-out.mp4" v:0 stream=width)" \
    "$(probe_value "$x265_source" v:0 stream=width)" "x265 candidate width"
assert_equal "$(probe_value "$test_dir/x265-out.mp4" v:0 stream=height)" \
    "$(probe_value "$x265_source" v:0 stream=height)" "x265 candidate height"
assert_equal "$(probe_value "$test_dir/x265-out.mp4" v:0 stream_side_data=rotation)" \
    "$(probe_value "$x265_source" v:0 stream_side_data=rotation)" "x265 candidate rotation"
assert_equal "$(probe_value "$test_dir/x265-out.mp4" '' format_tags=creation_time)" \
    "$(probe_value "$x265_source" '' format_tags=creation_time)" "x265 candidate creation_time"
assert_equal "$(probe_value "$test_dir/x265-out.mp4" '' format_tags=location)" \
    "$(probe_value "$x265_source" '' format_tags=location)" "x265 candidate location"
assert_equal "$(stream_count "$test_dir/x265-out.mp4" a)" \
    "$(stream_count "$x265_source" a)" "x265 candidate audio stream count"
grep -Eq 'frame threads / pool features[[:space:]]*: 1 /' "$test_dir/x265.log" ||
    fail "x265 did not allocate a pool within the configured thread count"
awk -v source="$(probe_value "$x265_source" '' format=duration)" \
    -v candidate="$(probe_value "$test_dir/x265-out.mp4" '' format=duration)" \
    'BEGIN { delta = source - candidate; if (delta < 0) delta = -delta; exit !(delta <= 0.6) }' ||
    fail "x265 candidate duration drifted beyond tolerance"

VIDEO_TRANSCODE_ENABLED=1 \
VIDEO_CODEC=libx265 \
VIDEO_PRESET=ultrafast \
VIDEO_BITRATE=300k \
VIDEO_THREADS=1 \
VIDEO_CONCURRENCY=1 \
VIDEO_LOCK_DIR="$test_dir/locks" \
QUIET_EXEC_BIN="$quiet_exec" \
QUIET_EXEC_SENSOR_ENABLED=0 \
    "$candidate" "$x265_source" "$test_dir/x265-bitrate-out" \
    2>"$test_dir/x265-bitrate.log"
assert_file "$test_dir/x265-bitrate-out.mp4"
assert_equal "$(probe_value "$test_dir/x265-bitrate-out.mp4" v:0 stream=codec_name)" hevc "x265 bitrate candidate codec"
assert_equal "$(sha256 "$x265_source")" "$x265_hash" "x265 bitrate source changed"

av1_source="$test_dir/source-subtitles.mkv"
av1_hash=$(sha256 "$av1_source")
VIDEO_TRANSCODE_ENABLED=1 \
VIDEO_CODEC=libsvtav1 \
VIDEO_PRESET=12 \
VIDEO_CRF=50 \
VIDEO_THREADS=1 \
VIDEO_CONCURRENCY=1 \
VIDEO_LOCK_DIR="$test_dir/locks" \
QUIET_EXEC_BIN="$quiet_exec" \
QUIET_EXEC_SENSOR_ENABLED=0 \
    "$candidate" "$av1_source" "$test_dir/av1-out" \
    2>"$test_dir/av1.log"
assert_file "$test_dir/av1-out.mkv"
assert_equal "$(sha256 "$av1_source")" "$av1_hash" "AV1 source changed"
assert_equal "$(probe_value "$test_dir/av1-out.mkv" v:0 stream=codec_name)" av1 "AV1 candidate codec"
assert_equal "$(stream_count "$test_dir/av1-out.mkv" a)" \
    "$(stream_count "$av1_source" a)" "AV1 candidate audio stream count"
assert_equal "$(stream_count "$test_dir/av1-out.mkv" s)" \
    "$(stream_count "$av1_source" s)" "AV1 candidate subtitle stream count"
assert_equal "$(probe_value "$test_dir/av1-out.mkv" s stream=codec_name)" ass "AV1 candidate subtitle codec"
assert_equal "$(probe_value "$test_dir/av1-out.mkv" '' format_tags=creation_time)" \
    "$(probe_value "$av1_source" '' format_tags=creation_time)" "AV1 candidate creation_time"
assert_equal "$(probe_value "$test_dir/av1-out.mkv" '' format_tags=location)" \
    "$(probe_value "$av1_source" '' format_tags=location)" "AV1 candidate location"

expect_failure mp4-subtitle-mux "$test_dir/mp4-subtitle-out" \
    env VIDEO_TRANSCODE_ENABLED=1 VIDEO_CODEC=libx265 VIDEO_PRESET=ultrafast \
        VIDEO_CRF=30 VIDEO_THREADS=1 VIDEO_CONCURRENCY=1 \
        VIDEO_LOCK_DIR="$test_dir/locks" QUIET_EXEC_BIN="$quiet_exec" \
        QUIET_EXEC_SENSOR_ENABLED=0 \
        "$candidate" "$av1_source" "$test_dir/mp4-subtitle-out"
assert_equal "$(sha256 "$av1_source")" "$av1_hash" "MP4 subtitle failure removed or changed the source"

hdr_source="$test_dir/source-hdr.mp4"
hdr_hash=$(sha256 "$hdr_source")
expect_failure hdr "$test_dir/hdr-out" \
    env VIDEO_TRANSCODE_ENABLED=1 VIDEO_CODEC=libx265 VIDEO_PRESET=ultrafast \
        VIDEO_CRF=30 VIDEO_THREADS=1 VIDEO_CONCURRENCY=1 \
        VIDEO_LOCK_DIR="$test_dir/locks" QUIET_EXEC_BIN="$quiet_exec" \
        QUIET_EXEC_SENSOR_ENABLED=0 \
        "$candidate" "$hdr_source" "$test_dir/hdr-out"
assert_equal "$(sha256 "$hdr_source")" "$hdr_hash" "HDR failure removed or changed the source"

printf 'video-candidate tests passed\n'
