# Video experiment configuration

The active storage-saver profile is unchanged. `tasks-disabled.yaml` is a
standalone snapshot with videos passed through unchanged and is the safe default.
`tasks-enabled.example.yaml` is an opt-in example for a future asynchronous
worker; it is not intended for a synchronous phone upload request.

The candidate script requires `VIDEO_CODEC`, `VIDEO_PRESET`, and exactly one of
`VIDEO_CRF` or `VIDEO_BITRATE` when `VIDEO_TRANSCODE_ENABLED=1`. Copy
`video.env.example` into your deployment environment, set the enabled flag only
after the worker path exists, and mount the chosen tasks file at
`/etc/immich-optimizer/config/storage-saver/tasks.yaml`.

See [the video pipeline design](../../docs/video-pipeline.md) for the validation
contract, retention rules, and the queued-worker prerequisites.
