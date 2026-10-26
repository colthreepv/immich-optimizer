// Package mobileview adapts response metadata for the stock Immich mobile app.
//
// The optimizer may store an optimized AVIF whose SHA-1 differs from the
// original upload. Immich v3.3.0 mobile compares response checksums with local
// original files, so this package can replace server checksums with the stored
// source SHA-1 while leaving unrelated response data alone.
//
// Lookup is called with the authenticated owner ID and asset ID. It returns the
// original SHA-1 as 40 hexadecimal characters. RewriteJSON and RewriteNDJSON
// encode that value as hexadecimal when the response checksum is hexadecimal,
// or as standard base64 otherwise. Base64 padding follows the original value.
//
// RewriteNDJSON supports the checksum-bearing asset event types emitted by
// Immich v3.3.0: AssetV2, PartnerAssetV2, PartnerAssetBackfillV2,
// AlbumAssetCreateV2, AlbumAssetUpdateV2, and AlbumAssetBackfillV2. Other sync
// records are preserved without semantic rewriting.
//
// RewriteBulkUploadCheckRequest translates source SHA-1 checksums to stored
// output SHA-1 checksums for the Immich v3.3.0
// POST /api/assets/bulk-upload-check request. That response has no checksum
// field, so callers forward upstream results unchanged and use
// ParseBulkUploadCheckResponse only to inspect the exact result DTO.
//
// Mapping data is supplied by the caller and must be indexed by authenticated
// owner and source SHA-1; this package retains no mappings.
package mobileview
