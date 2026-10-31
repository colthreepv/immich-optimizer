# Workstation Camera import

This is the legacy workstation experiment. It bypasses server-side conversion
and is not the ingestion path for the server/worker release. The experiment was
stopped; use the original-first proxy described in [async optimizer](async-optimizer.md).
The commands below remain as a record of the experiment.

Run `scripts/storage-saver/import-camera.ts` with Bun on Bangkokian. Originals
remain in place and are mounted read-only into isolated Docker converters.
No source backup, proxy configuration change, or live ledger mutation occurs.
Videos are rejected. The converter image defaults to
`colthreepv/immich-optimizer:storage-saver-ccc6b3e`, with sensor supervision
disabled explicitly, AVIF quality 50 / speed 6 / YUV420, one CPU per worker.
The existing converter validates dimensions, dates, GPS, orientation and ICC.
Motion/HDR/MPF rejection and outputs at least as large as their JPEG use the
original with identity provenance and an explicit reason. Other failures retry
three times and remain failed.

## Pilot

From the repository root, first prepare three files without uploading:

```powershell
$manifest = 'C:\Users\valerio\Documents\Codex\2026-10-26\immich-import-plan\camera_photos_sha1_sha256_manifest.jsonl'
$state = 'C:\Users\valerio\Documents\Codex\2026-10-30\immich-experiments\camera-import'
bun scripts/storage-saver/import-camera.ts --manifest $manifest --state $state --limit 3 --workers 3
```

The parent loads credentials into `IUO_IMMICH_API_KEY` and the **direct backend**
API origin into `IUO_IMMICH_URL` (without `/api`). Never use an optimizer URL:
checksum B must be checked without alias translation. Then upload the pilot:

```powershell
bun scripts/storage-saver/import-camera.ts --manifest $manifest --state $state --limit 3 --workers 3 --upload --download-verify
```

Review downloaded SHA-256 verification and the source/output records. The pilot
always downloads originals for SHA-1, SHA-256 and size verification. No uploads
occur without `--upload`; a limit above ten requires `--bulk-reviewed`.

## Resume and full run

After parent review and coordination with the video benchmark:

```powershell
bun scripts/storage-saver/import-camera.ts --manifest $manifest --state $state --limit 8180 --workers 4 --upload --bulk-reviewed
```

The limit selects the first N manifest rows, so increasing it preserves the
pilot. Every attempt validates local source SHA-1, SHA-256 and size, checks
current Immich source checksum A before conversion, then checks output checksum
B before upload. Cached conversions are hash-validated and reused; lost upload
acknowledgements recover via B. Existing originals use identity mappings.
Trashed duplicates, unknown responses, and conflicting provenance fail closed.
Uploads include version-one `storage-saver-source` metadata. Existing metadata
is compared field by field; it is never silently overwritten.

Each uploaded asset must report the authenticated owner, an active trash state,
and the actual output checksum. Server-computed file size must match when it is
available. Missing async metadata produces `server-checksum-pending-size`,
awaiting independent filesystem verification; it does not exhaust upload retries.
Bulk mode avoids full WAN downloads unless `--download-verify` is supplied.
It writes records in state `uploaded`, pending independent server-side SHA-256
verification by the parent. They must not be activated as verified aliases yet.

Per-source `prepared.json`, `converter.log` and `result.json` files survive
interruption. Final `results.json` includes output SHA-256 and originalPath;
`ledger-fragment.json` matches the Go ledger version-one schema. The parent
verifies server-side originals against those hashes and sizes, promotes only
confirmed records to `verified`, and merges with existing records while the
proxy is briefly stopped. Preserve existing source and asset mappings; reject
conflicts. This importer does not replace the live ledger or rescan the API.

Each successful file is journaled atomically in its `result.json` before progress
is printed. A resumed upload validates the owner and source manifest hashes and
reuses completed checkpoints without per-file API calls, including pending
server verification. Use `--recheck` to explicitly revisit remote state. The
parent can collect per-source results if interruption prevents final aggregation.

Only one process can use a state directory. A forced termination can leave
`import.lock`: inspect its PID and confirm no importer/container is running
before removing that lock. Completed checkpoints describe the observed state at
the time of import; final server verification remains the parent's responsibility.
Do not delete staged output until the parent has completed verification/merge.

Run importer tests with `bun test scripts/storage-saver/import-camera.test.ts`.

## Performance review during the eight-worker run

On 2026-10-30, a separate traced conversion of `IMG_20230408_181453.jpg`
used the same image, one-CPU limit and preset while the import continued.
Total wall time was 5.87 seconds; encoding accounted for 4.01 seconds,
metadata extraction about 1.4 seconds, and container/CLI overhead about
0.38 seconds. This is one sample under concurrent load, not an overall ETA.

A separate three-repeat metadata benchmark compared fourteen scalar ExifTool
processes (ten source tags and four output tags) against two batched reads
of those same tags. Scalar reads took 1.546 / 2.013 / 1.595 seconds; batched
reads took 0.176 / 0.224 / 0.222 seconds. Binary XMP/ICC extraction, unsupported
feature rejection, dimensions, transform checks and all preservation assertions
must remain when implementing batching. No batching change has been deployed.

Three `docker run --rm ... true` probes took 0.550 / 0.416 / 0.417 seconds.
Persistent worker containers could amortize some of that overhead, but the
larger measured opportunity is reducing repeated ExifTool startup. A later
implementation should keep one bounded container per worker, its input mount
read-only, and process cleanup confined to its own jobs. Neither optimization
requires a quality or preset change. Both need representative end-to-end
validation before restarting the importer; larger images may be dominated by
encoding and benefit much less.

The current process and importer source were left unchanged during profiling.
Evidence lives outside the repository under
`C:\Users\valerio\Documents\Codex\2026-10-30\immich-experiments\import-profile`.
Completed checkpoints continue to work after a parent ledger merge: resume
skips their API calls, while an incomplete upload checks direct-backend B.

## Opt-in batched converter

`scripts/storage-saver/import-camera-convert.sh` implements the same encoder
invocation and preservation assertions with two batched ExifTool JSON reads.
Perl core JSON::PP/MIME::Base64 decode binary XMP/ICC; source feature checks run
before encoding. Dates, offsets, GPS and ICC bytes are compared afterward,
with the existing dimensions, YUV420, color signals and orientation checks.
The image, preset identifier and provenance contract are unchanged.

After parent review, stop/drain the current importer and resume the same command
and state directory with this additional explicit option:

```powershell
--converter-script 'C:\Users\valerio\projects\valerio\immich-optimizer\scripts\storage-saver\import-camera-convert.sh'
```

The importer mounts that file read-only at `/converter.sh` and invokes it through
Bash. Omitting the option retains the original converter. Existing prepared
outputs and successful checkpoints are reused. No live process changes its
converter automatically. The parent coordinates draining in-flight containers
and releasing the state lock before restarting.

Verification: three retained pilot outputs and five additional 20 MP images
(5184x3888 or 3888x5184) all produced byte-identical SHA-256 outputs versus the
original converter, with jobs=1 and identical encoder flags. Five sequential
paired runs, alternating old/new order while the bulk import continued, took
34.51 seconds old versus 23.72 seconds new (31% less wall time). This measures
the sample, not an assured improvement to eight-worker aggregate throughput.
Evidence: `immich-experiments\import-batch-profile\paired-timings.json`.

Run the 46 metadata validator cases in the existing image:

```powershell
docker run --rm --network none --mount 'type=bind,source=C:\Users\valerio\projects\valerio\immich-optimizer,target=/repo,readonly' colthreepv/immich-optimizer:storage-saver-ccc6b3e bash /repo/scripts/storage-saver/import-camera-convert.test.sh
```

The cases include altered/missing date, offset, GPS and ICC, every EXIF
orientation, absent/mismatched AVIF transforms, dimensions, YUV and color
signals, MPF, textual/base64 motion/HDR markers and malformed binary metadata.

## Opt-in persistent worker pool

Add `--persistent-workers` to a future parent-controlled run to allocate exactly
`--workers N` reusable Docker containers instead of starting one per conversion.
The default command path remains `docker run --rm` and requires no migration.
The converter script is unchanged; `--converter-script` works with either mode.
This feature has not been adopted by the ongoing import or its supervisor.

Each numbered slot runs at most one `docker exec` conversion at a time, with
one CPU, 64 PIDs, networking disabled, and 1024 MiB memory including swap.
Use `--pool-memory-mb` (128–4096) to change that per-container memory ceiling;
`--worker-timeout-seconds` (1–3600, default 300) bounds each conversion attempt.
The encoder remains jobs=1 at the same quality, speed and chroma settings.
Eight idle containers still have overhead, and the aggregate memory allowance
is N times the per-container limit. Neither a speedup nor a memory reduction
has been established for this mode. The default importer already bounds the
number of conversion jobs; a fixed pool alone is not proof of leak prevention.

Pool names derive from the canonical state directory's SHA-256 and slot number.
The existing exclusive `import.lock` is held throughout startup and cleanup.
Before allocating anything, startup reconciles stale containers bearing the
pool's exact kind/state/slot labels, including slots from a previously larger
pool. New containers also carry a unique run label. Cleanup inspects ownership
and removes by immutable Docker ID, never by broad name matching. Foreign or
unverifiable containers are not removed and block startup. Do not manually
launch two pools for one state directory or remove a live process's lock.

A timeout or failed exec retires the entire slot container, killing any
server-side encoder left behind by its Docker client. Explicit unsupported
motion/HDR/MPF rejection is returned to the existing identity-fallback logic
without replacement. A replacement starts at the same slot name only after
removal succeeds and a fresh inspection confirms absence. Each slot permits
two replacements over the entire run; lifecycle uncertainty or exhausted
budget stops further scheduling. Startup failure, normal completion, SIGINT
and SIGTERM clean only containers owned by that run. Active HTTP operations
may finish before shutdown completes; their checkpoint logic is unchanged.

Cleanup failure retains `import.lock` as a recovery fence and reports an error.
After a forced kill, inspect the recorded PID and labelled slot containers;
only when no importer owns the state may the parent release a stale lock and
restart. Startup will reconcile its old containers before allocating new ones.
An unavailable Docker daemon cannot be treated as confirmation of removal.

Mounts use canonical paths: the selected sources' common directory is read-only,
the state directory is writable, and an optional converter script is read-only.
Filesystem-root mounts, source/stage overlap, scripts inside writable staging,
comma-containing mount paths, and per-job paths escaping their roots are
rejected. This requires a common non-root source directory; cross-drive
manifests should be split into separate runs/state directories.

Run fake-Docker lifecycle tests without starting containers:

```powershell
bun test scripts/storage-saver/import-camera.test.ts scripts/storage-saver/import-camera-pool.test.ts
```

Tests cover live-container bounds, repeated jobs, stale reconciliation, timeout
and crash replacement ordering, replacement budgets, failed/unconfirmed removal,
partial startup, ownership refusal and concurrent close. Native Docker pool
startup, signal delivery and real conversion remain unvalidated; no second pool
was launched alongside the CPU-saturated import. Parent review and a small
isolated native smoke run are required before adopting the opt-in mode.
