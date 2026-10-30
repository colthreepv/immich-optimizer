# Async optimizer

This describes the original-first path under development. It is intended for a single Immich owner, with one server and a pull worker. It does not import existing libraries or ledger entries in bulk.

Original-first is opt-in; the existing storage-saver mode remains available. The planned Carlino server explicitly sets `IUO_ORIGINAL_FIRST=true`. The initial phone gate is a read-only check of settings and sync behavior. Keep `IUO_RETIRE_ORIGINAL` disabled until phone validation is complete. Before enabling it, turn off Sync Remote Deletions on every phone. In Immich Android 3.3.4, this setting is under Advanced. The server does not enforce the phone setting.

## Configuration

The planned image is `ghcr.io/colthreepv/immich-optimizer:v0.4.0-rc.2`. Its default command is `immich-optimizer`. Override the command with `optimizer-worker` for the worker. Both commands use the same image.

Server settings for original-first mode:

| Environment variable | Purpose |
| --- | --- |
| `IUO_MODE=proxy` | Run the public proxy. |
| `IUO_ORIGINAL_FIRST=true` | Enable original-first ingestion. |
| `IUO_IMMICH_URL` | Immich server URL. |
| `IUO_IMMICH_API_KEY` or `IUO_IMMICH_API_KEY_FILE` | API key for the same owner as the uploads. Set one source only. |
| `IUO_STATE_DIR` | Persistent directory for the alias ledger, queue, and staged files. Keep this across restarts. |
| `IUO_WORKER_TOKEN` or `IUO_WORKER_TOKEN_FILE` | Shared worker API token. Set one source only. |
| `IUO_PROXY_BIND_ADDR` | Public proxy address. Default `:8080`. |
| `IUO_WORKER_BIND_ADDR` | Internal worker API address. Default `:8082`. |
| `IUO_PRESET_ID` | Preset identifier. Default `avif-q50-s6-420-v1`. |
| `IUO_MOBILE_VIEW=true` | Enable the experimental original-checksum view. |
| `IUO_RETIRE_ORIGINAL` | Allow worker claims and verified original retirement. Defaults to `false`. |
| `IUO_QUEUE_MAX_JOBS` | Historical job admission limit. Default `10000`. |
| `IUO_QUEUE_MAX_SPOOL_BYTES` | Spool admission limit in bytes. Default `107374182400` (100 GiB). |

The image supplies the storage-saver tasks file. The server needs access to Immich and a writable persistent state directory. The worker API is separate from the public proxy; make it reachable by authorized workers.

Worker settings:

| Setting | Purpose |
| --- | --- |
| `IUO_SERVER_URL` or `-server-url` | URL of the server's worker API, such as `http://optimizer:8082`. |
| `IUO_WORKER_TOKEN` or `IUO_WORKER_TOKEN_FILE` | The server's shared worker token. Set one source only. |
| `IUO_WORKER_ID` or `-worker-id` | Worker identity. Defaults to the hostname. |
| `-converter` | Converter executable. Defaults to `jpeg-to-avif.sh`. |
| `-temp-dir` | Temporary directory for downloaded source and output. |

The worker health endpoint is `http://127.0.0.1:8081/health`. `optimizer-worker health` checks it locally. Override the image's server health check for a worker container. The endpoint reports the worker process state, not successful conversion or publication.

## Processing and limits

The server queues eligible JPEG uploads before forwarding originals to Immich. It acknowledges a successful new upload only with an actual Immich asset ID. If the queue cannot record that ID, the server logs deferred optimization and can reconcile the intent later. Other formats pass through as originals. The worker currently converts `.jpg` and `.jpeg` with `avif-q50-s6-420-v1`; unsupported content and output that is not smaller are skipped.

With retirement disabled, worker claims are paused and source intents stay queued. With `IUO_RETIRE_ORIGINAL=true`, workers can process jobs and the publisher uploads the AVIF as a new asset, verifies it, migrates supported relationships and metadata, and records the original SHA-1 to replacement ID alias. After the ledger write, it requests a fresh Immich sync update for the replacement and records that step before trashing the original. Unsupported relationships or uncertain verification hold the job for review. Held jobs need separate review and resumption. Real Immich fixture validation is still pending.

The queue stores an atomically replaced JSON snapshot with a single writer, not SQLite. Use a local durable filesystem for the state directory. Cleanup releases terminal job spool bytes only after checking the active original in Immich for skipped or failed jobs, or the verified replacement for completed jobs. Held jobs retain their bytes for review. The default limits are 100 GiB of spool data and 10,000 historical jobs. Cleanup frees spool space but retains job records, so it does not free historical job slots. When admission fails, the server logs missed optimization and attempts a normal original upload to Immich.

Trashing does not immediately free disk space. Mobile upload, sync, and deletion still require end-to-end validation before local originals are deleted. Preserve the existing assets and alias ledger while testing.
