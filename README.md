# Immich Optimizer

Immich Optimizer is an independent project maintained at [colthreepv/immich-optimizer](https://github.com/colthreepv/immich-optimizer). Its original-first proxy sends the uploaded original to [Immich](https://immich.app/) and queues eligible JPEGs for later AVIF conversion. A successful new upload response contains a real Immich asset ID. Duplicate responses use an existing asset ID. If recording the original ID in the queue fails, the server logs the deferred optimization and can reconcile the intent later.

This work is experimental. Synthetic fixtures and a real-JPEG load test on Immich 3.3.5 have verified replacement, metadata migration, checksum sync and queue cleanup. Validation on the actual mobile device remains pending. Original retirement is disabled by default; while disabled, worker claims are paused and source intents stay queued.

Original-first ingestion is opt-in. The existing storage-saver mode remains available. Set `IUO_ORIGINAL_FIRST=true` and leave `IUO_RETIRE_ORIGINAL` disabled until phone validation is complete.

Before enabling retirement, turn off Sync Remote Deletions on every phone. In Immich Android 3.3.4, this setting is under Advanced. The server does not enforce this setting.

The release image is `ghcr.io/colthreepv/immich-optimizer:v0.4.0-rc.6`, for Linux amd64. The same image runs the server with `immich-optimizer` and the worker with `optimizer-worker`. The image contains the JPEG-to-AVIF preset `avif-q50-s6-420-v1` and libavif 1.2.1.

The server's public proxy listens on port 8080 by default. Its separate worker API listens on port 8082. The worker exposes its local health endpoint on `127.0.0.1:8081/health`; use `optimizer-worker health` for a container health check. The image's default health check targets the server and needs an override for the worker.

See [architecture](ARCHITECTURE.md) for the data flow and [async optimizer](docs/async-optimizer.md) for configuration and current limits.

Remote conversion workers can use the persistent [worker Compose configuration](compose.worker.yaml). See the [remote-worker guide](docs/remote-worker.md) for private API access, token configuration and resource limits.

## License and attribution

This project builds on [miguelangel-nubla/immich-optimizer](https://github.com/miguelangel-nubla/immich-optimizer). The original copyright notice and MIT terms remain in [LICENSE](LICENSE).

During the initial rollout, automatic GitHub push and release event delivery was not observed. The fixed-tag workflow passes with an explicit dispatch. For a new tag whose image has not been published, use:

```powershell
gh workflow run release.yaml --repo colthreepv/immich-optimizer --ref TAG
```

Use a new tag instead of rebuilding an already published image.
