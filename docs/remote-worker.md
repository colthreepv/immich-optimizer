# Remote conversion worker

`compose.worker.yaml` is an example for one persistent JPEG conversion worker on Bangkokian. It pulls jobs from the optimizer server's private worker API and writes only to a named scratch volume. It needs no Immich API key or source-library mount.

## Required configuration

Set these values in the deployment shell or the local Compose `.env` file outside this repository:

| Variable | Purpose |
| --- | --- |
| `IUO_SERVER_URL` | Tailscale-only HTTPS base URL. Ingress routes only `/internal/jobs*` to the worker API on port 8082. Never use the public proxy URL. |
| `IUO_WORKER_TOKEN_PATH` | Host path to a file containing the shared `immich-optimizer-worker-v1` token. |
| `IUO_WORKER_ID` | Optional unique worker ID; defaults to `bangkokian-1`. |
| `IUO_WORKER_IMAGE` | Optional image override; defaults to `ghcr.io/colthreepv/immich-optimizer:v0.4.0-rc.6`. |
| `IUO_WORKER_GODEBUG` | Optional Go runtime settings. `http2client=0` uses HTTP/1.1 if bulk downloads stall heartbeat responses over the private ingress. Empty by default. |

Use a generic endpoint such as `https://photos.example.invalid`. Tailscale provides transport access through the private entrypoint; do not publish the worker API on the public internet. The endpoint must not include `/internal/jobs`.

The token file is mounted through a Compose secret as `IUO_WORKER_TOKEN_FILE`. Keep the file outside the repository and protect its permissions. Compose requires both the endpoint variable and token-path variable to be set.

## Example workflow

The repository's `compose.worker.yaml` is the canonical local config; do not copy it. Keep the environment file and token outside the repository. On Windows, put `worker.env` under `$env:LOCALAPPDATA\immich-optimizer-worker` and place `worker-token` beside it. Validate without starting containers:

```powershell
$envFile = Join-Path $env:LOCALAPPDATA 'immich-optimizer-worker\worker.env'
docker compose --env-file $envFile -f compose.worker.yaml config
```

After the private environment file contains the final `IUO_SERVER_URL` and `IUO_WORKER_TOKEN_PATH`, start the worker with:

```powershell
docker compose --env-file $envFile -f compose.worker.yaml up -d
```

The worker uses `restart: unless-stopped`, a persistent `worker-scratch` named volume at `/work`, 2 GiB memory, a 128-process limit, and a 30-second stop grace period. A one-shot init service sets the volume owner before the worker starts. `optimizer-worker health` is the container health check. It reports process health, not successful conversion or publication.

The example runs one serial worker: the optimizer claims one conversion at a time and `AVIF_JOBS=1` prevents an encoder fan-out. Add replicas only after validating server claims, CPU load, and scratch capacity.

For the current Bangkokian deployment, HTTP/1.1 keeps lease-heartbeat requests on a separate connection from a long source download. It does not remove the bandwidth and latency cost of transferring source files to a remote worker.

`QUIET_EXEC_SENSOR_ENABLED=0` disables the Carlino fan-sensor watchdog because Bangkokian exposes a different sensor environment. The example deliberately sets no `TZ`; conversion metadata and timestamps are handled explicitly, while container logs use the image default.

This JPEG-to-AVIF conversion uses CPU with the bundled `avif-q50-s6-420-v1` preset. The one GPU ML container on Bangkokian is a separate Immich/ML service for machine-learning inference. Do not add a GPU reservation to this worker or treat both services as one process.

## Checks

- `docker compose -f compose.worker.yaml config` renders with the required variables and resolves the token path.
- The worker has no published ports, no Immich API key, and no source backup mount.
- The worker API remains reachable only through private Tailscale access.
