# Storage saver experiment

The optional storage-saver proxy accepts ordinary Immich uploads, transforms
JPEGs into full-resolution AVIFs, and stores the *actual output* checksum in
Immich. It never edits Immich's database or managed files.

## Ingestion contract

1. Resolve the authenticated user through `/api/users/me`.
2. Persist the source and its SHA-1 in a dedicated staging volume.
3. Convert according to a versioned, fixed preset; retain the original if the
   output is not smaller. Store the converted bytes before sending to Immich.
4. Preserve upload timestamps, sidecars, client metadata and other form fields.
   Send the correct filename and MIME type and remove obsolete checksum headers.
5. Upload with custom `storage-saver-source` metadata, then download the original
   from Immich and verify its size and SHA-1 against the staged output.
6. Persist the asset ID and both hashes; ensure the metadata backup exists.
   Only then acknowledge the upload. A retry reuses staged output. A verified
   duplicate is checked against Immich before returning its asset ID.

The persistent ledger is owner-scoped; another user's source hash cannot expose
an asset. Corrupt state is an error, never an instruction to reset the cache.
Run one replica with upload concurrency one. The JSON ledger is intended for this
pilot; it is not a multi-process database. Back up the complete state volume.

Staged sources and outputs are deliberately retained in this experiment, even
after successful upload. Disk reclamation needs a separate verified cleanup
policy. Failed uploads must never trigger automatic deletion of source files.

## Mobile experiment

`IUO_MOBILE_VIEW=true` enables owner-scoped response translation: supported JSON
asset responses and Immich v3.3.0 sync events present source checksum A to the
client while Immich retains output checksum B. Downloads return the actual
stored bytes. This is an experimental protocol adapter, not proof that stock
mobile backup, restore, local deletion and retry behavior all work end to end.
Use a separate hostname until those scenarios have been checked on a phone.

The default is off. Client upgrades require protocol revalidation. Metadata
rewrites never substitute another owner's checksum. Unknown sync events remain
unchanged. There is no modified mobile app.

## Conversion and fan control

The preset is AVIF quality 50, speed 6, 4:2:0, original dimensions. The codec's
quality scale must match the encoder version: legacy quantizer 50 is **not**
equivalent to quality 50. See the container recipe and preset script for the
exact implementation. AVIF and other configured image formats pass through;
videos pass through until a video recipe is explicitly selected and validated.

The `quiet-exec` supervisor reads a named hwmon fan sensor and pauses only the
encoder's process group. Default thresholds are pause above 3000 RPM, resume
below 2600 RPM after 60 seconds. Missing sensors block conversion when enabled.
These are configurable thresholds, not a guarantee of silence; firmware,
unrelated services and sensor calibration also affect fan behavior. Do not
pause the whole proxy or Immich container.

For initial bulk ingestion, the same converter can run on a workstation with
fan supervision disabled explicitly, writing to a staging directory. Upload
those AVIFs through this proxy as passthrough or directly to Immich. Direct
uploads do not automatically gain original-JPEG checksum aliases; import the
verified provenance mapping before expecting a phone to recognize those JPEGs.

## Boundaries

- Uploads are synchronous and bounded at 2 GiB; there is no durable background
  queue or early success acknowledgement.
- HTTP/upload timeout and mobile operating-system background limits still
  apply while an encoder waits for the fan. Retrying preserves staged work.
- Existing server-side album membership, edits and descriptions are not migrated
  by this ingestion experiment. It creates assets and does not delete originals.
- No production hostname replacement, bulk library migration, video recompression
  or Google Photos deletion is implied by deployment of the experimental proxy.

Official protocol reference: [Immich v3.3.0 OpenAPI](https://github.com/immich-app/immich/blob/v3.3.0/open-api/immich-openapi-specs.json).
