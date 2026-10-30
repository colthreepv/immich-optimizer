# Workstation Camera import

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
