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
the actual output checksum and exact server-computed file size. If extraction
has not populated file size yet, a subsequent bounded retry checks it again.
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

Only one process can use a state directory. A forced termination can leave
`import.lock`: inspect its PID and confirm no importer/container is running
before removing that lock. Repeating the command rechecks current server state;
it does not trust an old success to imply that the remote asset still exists.
Do not delete staged output until the parent has completed verification/merge.

Run importer tests with `bun test scripts/storage-saver/import-camera.test.ts`.
