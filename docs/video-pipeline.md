# Experimental video candidate pipeline

Status: **preparation only**. Video transcoding is disabled by default, the
active storage-saver profile still passes videos through unchanged, and no
mobile, proxy, store, watchdog, or deployment configuration is changed by this
work.

## Why this is not enabled yet

The current phone upload path is synchronous. Encoding a large video inside the
upload request would tie request success to encoder speed, progress reporting,
and fan pauses. That is an architectural warning for any synchronous upload
path, independent of a particular server or timeout value. Enabling this recipe
in production needs a durable job queue and a separate worker first. The current
candidate script is the worker-side building block, not a mobile replacement.

## Default and opt-in profiles

`config/video-experimental/tasks-disabled.yaml` is a standalone snapshot of the
storage-saver profile with a passthrough video task. With
`VIDEO_TRANSCODE_ENABLED=0` (the default), the candidate script copies the input
bytes to a separate candidate path and never invokes FFmpeg. The original path is
never unlinked by the script.

`config/video-experimental/tasks-enabled.example.yaml` is an opt-in example for
a future worker. Mounting it does not enable lossy output by itself. When
`VIDEO_TRANSCODE_ENABLED=1`, the script requires all of these to be explicit:

- `VIDEO_CODEC` is `libx265` or `libsvtav1`.
- `VIDEO_PRESET` is a valid x265 preset or a libsvtav1 preset from 0 to 13.
- Exactly one of `VIDEO_CRF` or `VIDEO_BITRATE` is set.

There is no built-in quality value. A missing codec, preset, or quality setting
fails before encoding. The script also refuses an unknown codec, an out-of-range
CRF, an invalid preset, or both CRF and bitrate at once.

`libx265` writes an MP4 candidate and tags HEVC as `hvc1`. `libsvtav1` writes an
MKV candidate so subtitles and less common streams have room to be copied.
Immich lists both MP4 and Matroska (`.mkv`) as supported video formats in its
[supported media formats](https://immich.app/docs/features/supported-formats/)
documentation. The pipeline uses the candidate extension only after the input
has been verified as a real video; the output filename is derived from the
actual candidate.

## Candidate contract

The script takes `INPUT_VIDEO` and `OUTPUT_PREFIX`. The final path is
`OUTPUT_PREFIX.<candidate-extension>`; the prefix must live in a writable
directory separate from the source. Temporary work stays below that directory.
The script publishes the candidate only after probing and validation succeed.

The source is read-only. The script never deletes, renames, or rewrites the
input. If the pipeline rejects a smaller-video decision, it retains the original
through the existing pipeline rule: a processed file is uploaded only when its
size is strictly smaller than the original size. A candidate that is equal or
larger leaves the original as the upload choice.

The FFmpeg recipe maps every stream (`-map 0`), copies chapters and metadata,
copies audio, subtitles, and auxiliary streams, and re-encodes only video stream
zero. It uses `-noautorotate` so the display rotation matrix stays attached to
the re-encoded frames. MP4 fast-start is enabled for x265. The encoder process
runs through `quiet-exec`, which supervises the process group; the fan policy
continues to pause above 3000 RPM and resume below 2600 RPM after 60 seconds.

## Validation and fail-closed rules

The input must probe as a real video with exactly one video stream. The output
must have the expected codec, one video stream, the same coded width and height,
the same rotation side data, a duration within the configured tolerance, and the
same creation time when present. Location metadata is compared when the source
has a location tag. The non-video stream signature (type, codec, language,
default and forced disposition) must match exactly, so an audio or subtitle
stream cannot disappear silently.

`VIDEO_DURATION_TOLERANCE_SECONDS` defaults to `0.5`. The other checks are
exact. If the destination container cannot carry a source subtitle codec, the
mux fails and no candidate is published; this is intentional. Examples include
ASS subtitles in an MP4 output, which fail rather than being dropped or altered.

The input is rejected before encoding when it signals HDR, Dolby Vision, motion
photo, spatial/stereoscopic video, or dynamic HDR side data. The output is
checked again for those markers. HDR is identified from PQ/HLG transfer
characteristics, BT.2020 primaries, and HDR/DOVI side data. These cases need a
deliberate color-preservation recipe and are outside this preparation.

## Process and concurrency controls

- `VIDEO_THREADS` defaults to `1` and is passed to FFmpeg; x265 also gets
  `pools=<VIDEO_THREADS>:frame-threads=<VIDEO_THREADS>`, and libsvtav1 gets
  `lp=<VIDEO_THREADS>`.
- `VIDEO_CONCURRENCY` defaults to `1` and is enforced with `flock` slots below
  `VIDEO_LOCK_DIR` (default `/tmp/immich-optimizer-video-slots`).
- No CPU quota, aggressive `nice` level, GPU path, or mobile fork is introduced.
- The existing quiet-exec sensor behavior stays in control of pauses. The
  defaults target a modest CPU and keep the existing 3000 RPM pause threshold.

The pipeline's own `IUO_MAX_CONCURRENT_REQUESTS` remains a separate limit. A
future worker should set both the queue worker count and `VIDEO_CONCURRENCY`
deliberately so request concurrency does not become encoder concurrency.

## Files

- `scripts/storage-saver/video-candidate.sh` - opt-in candidate worker helper.
- `scripts/storage-saver/video-candidate.test.sh` - synthetic media tests.
- `config/video-experimental/tasks-disabled.yaml` - safe passthrough profile.
- `config/video-experimental/tasks-enabled.example.yaml` - future opt-in example.
- `config/video-experimental/video.env.example` - environment template.
- `Dockerfile.storage-saver` copies only the new worker script in addition to the
  existing storage-saver helpers. The active image environment remains disabled.

## Tests

Run the focused test suite in a Debian-based FFmpeg environment. The repository's
storage-saver test image already has FFmpeg 7.1.5, libx265, libsvtav1, util-linux,
and the existing quiet-exec helper:

```sh
docker run --rm \
  -v "$PWD:/repo" -w /repo \
  --entrypoint bash \
  immich-optimizer-storage-saver:test \
  scripts/storage-saver/video-candidate.test.sh
```

The tests generate synthetic videos with audio, ASS subtitles, rotation,
creation time, and location. They verify the disabled byte-for-byte copy without
calling FFmpeg, required opt-in parameters, x265 MP4 success with preserved
metadata, AV1 MKV success with audio and subtitles, fail-closed MP4 subtitle
muxing, HDR rejection, source checksums, and duration/resolution/rotation
validation.

## Enabling checklist for a future change

1. Add a durable upload-to-job handoff and return the mobile request before
   encoding starts.
2. Add a worker with bounded concurrency, job retry state, and a timeout that
   covers fan pauses and large videos.
3. Keep the candidate and original paths separate; retain originals and report
   the candidate/original ratio before any library migration.
4. Exercise real iPhone, Android, HDR, spatial, subtitle, audio, and retry cases
   against a disposable Immich instance.
5. Keep `VIDEO_TRANSCODE_ENABLED=0` until the fail-closed tests and the worker's
   provenance/ledger behavior have been reviewed.

This document records preparation and validation only. It does not authorize a
deployment, a source deletion, or a change to the active storage-saver profile.
