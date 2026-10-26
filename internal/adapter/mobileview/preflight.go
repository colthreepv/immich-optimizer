package mobileview

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// SourceOutputLookup resolves an authorized owner's source SHA-1 to the SHA-1
// currently stored by Immich. Both values are 40 hexadecimal characters.
//
// The durable index belongs to the caller. This package does not retain request
// data or checksum mappings.
type SourceOutputLookup func(ownerID, sourceSHA1Hex string) (outputSHA1Hex string, ok bool)

// RewriteBulkUploadCheckRequest translates source checksums in an Immich
// v3.3.0 POST /api/assets/bulk-upload-check request.
//
// Immich's request DTO is:
//
//	{"assets":[{"id":"client-id","checksum":"source-sha1"}]}
//
// Each item checksum is decoded as SHA-1, looked up by the authenticated owner,
// and replaced with the output SHA-1 in the same hex or standard-base64 form.
// Item IDs, ordering, and unrelated fields are preserved. An unmapped or
// unsupported checksum is left unchanged. Lookup must return a valid SHA-1
// hexadecimal digest when ok is true; an invalid result is an error.
//
// When no checksum changes, the original data is returned unchanged.
func RewriteBulkUploadCheckRequest(data []byte, ownerID string, lookup SourceOutputLookup) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("decode bulk upload check request: %w", err)
	}
	if root == nil {
		return nil, errors.New("bulk upload check request must be an object")
	}

	assetsJSON, ok := root["assets"]
	if !ok {
		return nil, errors.New("bulk upload check request is missing assets")
	}
	var assets []json.RawMessage
	if err := json.Unmarshal(assetsJSON, &assets); err != nil {
		return nil, fmt.Errorf("decode bulk upload check assets: %w", err)
	}
	if assets == nil {
		return nil, errors.New("bulk upload check assets must be an array")
	}

	changed := false
	for index, rawItem := range assets {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, fmt.Errorf("decode bulk upload check asset %d: %w", index, err)
		}
		if item == nil {
			return nil, fmt.Errorf("bulk upload check asset %d must be an object", index)
		}

		id, err := requiredString(item, "id")
		if err != nil {
			return nil, fmt.Errorf("bulk upload check asset %d: %w", index, err)
		}
		checksum, err := requiredString(item, "checksum")
		if err != nil {
			return nil, fmt.Errorf("bulk upload check asset %d: %w", index, err)
		}

		sourceSHA1, encoding, supported := decodeChecksumSHA1(checksum)
		if !supported || lookup == nil {
			continue
		}
		outputSHA1, found := lookup(ownerID, sourceSHA1)
		if !found {
			continue
		}
		if !isHexSHA1(outputSHA1) {
			return nil, fmt.Errorf("asset %q: lookup returned invalid output SHA-1 hex %q", id, outputSHA1)
		}

		replacement := encodeChecksumSHA1(outputSHA1, encoding)
		if replacement == checksum {
			continue
		}
		encodedChecksum, err := json.Marshal(replacement)
		if err != nil {
			return nil, fmt.Errorf("asset %q: encode output checksum: %w", id, err)
		}
		item["checksum"] = encodedChecksum
		rewrittenItem, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("asset %q: encode item: %w", id, err)
		}
		assets[index] = rewrittenItem
		changed = true
	}

	if !changed {
		return data, nil
	}
	rewrittenAssets, err := json.Marshal(assets)
	if err != nil {
		return nil, fmt.Errorf("encode bulk upload check assets: %w", err)
	}
	root["assets"] = rewrittenAssets
	rewritten, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode bulk upload check request: %w", err)
	}
	return rewritten, nil
}

// BulkUploadCheckResult is one result in an Immich v3.3.0
// POST /api/assets/bulk-upload-check response.
type BulkUploadCheckResult struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	AssetID   string `json:"assetId,omitempty"`
	IsTrashed *bool  `json:"isTrashed,omitempty"`
}

// ParseBulkUploadCheckResponse decodes Immich v3.3.0 bulk upload check results.
//
// The response contains no checksum field: upstream results should be forwarded
// unchanged so the client IDs, actions, rejection reasons, asset IDs, ordering,
// and unknown future fields survive. This parser provides a typed view for tests
// and integration checks without modifying the response.
func ParseBulkUploadCheckResponse(data []byte) ([]BulkUploadCheckResult, error) {
	var response struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("decode bulk upload check response: %w", err)
	}
	if response.Results == nil {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(data, &root); err != nil || root == nil {
			return nil, errors.New("bulk upload check response must be an object")
		}
		if _, ok := root["results"]; !ok {
			return nil, errors.New("bulk upload check response is missing results")
		}
		return nil, errors.New("bulk upload check results must be an array")
	}

	results := make([]BulkUploadCheckResult, len(response.Results))
	for index, rawResult := range response.Results {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(rawResult, &object); err != nil {
			return nil, fmt.Errorf("decode bulk upload check result %d: %w", index, err)
		}
		if object == nil {
			return nil, fmt.Errorf("bulk upload check result %d must be an object", index)
		}

		id, err := requiredString(object, "id")
		if err != nil {
			return nil, fmt.Errorf("bulk upload check result %d: %w", index, err)
		}
		action, err := requiredString(object, "action")
		if err != nil {
			return nil, fmt.Errorf("bulk upload check result %d: %w", index, err)
		}
		if action != "accept" && action != "reject" {
			return nil, fmt.Errorf("bulk upload check result %d has invalid action %q", index, action)
		}

		result := BulkUploadCheckResult{ID: id, Action: action}
		if reason, present, err := optionalString(object, "reason"); err != nil {
			return nil, fmt.Errorf("bulk upload check result %d: %w", index, err)
		} else if present {
			if reason != "duplicate" && reason != "unsupported-format" {
				return nil, fmt.Errorf("bulk upload check result %d has invalid reason %q", index, reason)
			}
			result.Reason = reason
		}
		if assetID, present, err := optionalString(object, "assetId"); err != nil {
			return nil, fmt.Errorf("bulk upload check result %d: %w", index, err)
		} else if present {
			result.AssetID = assetID
		}
		if rawTrashed, present := object["isTrashed"]; present {
			var isTrashed bool
			if err := json.Unmarshal(rawTrashed, &isTrashed); err != nil {
				return nil, fmt.Errorf("bulk upload check result %d: isTrashed must be a boolean", index)
			}
			result.IsTrashed = &isTrashed
		}
		results[index] = result
	}
	return results, nil
}

func optionalString(object map[string]json.RawMessage, key string) (string, bool, error) {
	raw, present := object[key]
	if !present {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, fmt.Errorf("%s must be a string", key)
	}
	return value, true, nil
}
func requiredString(object map[string]json.RawMessage, key string) (string, error) {
	raw, ok := object[key]
	if !ok {
		return "", fmt.Errorf("missing %s", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return value, nil
}

type checksumEncoding struct {
	base64    bool
	rawBase64 bool
	upperHex  bool
}

func decodeChecksumSHA1(checksum string) (string, checksumEncoding, bool) {
	if len(checksum) == sha1.Size*2 {
		decoded, err := hex.DecodeString(checksum)
		if err == nil && len(decoded) == sha1.Size {
			return hex.EncodeToString(decoded), checksumEncoding{upperHex: strings.ContainsAny(checksum, "ABCDEF")}, true
		}
	}

	rawBase64 := len(checksum) == base64.RawStdEncoding.EncodedLen(sha1.Size)
	decoder := base64.StdEncoding
	if rawBase64 {
		decoder = base64.RawStdEncoding
	} else if len(checksum) != base64.StdEncoding.EncodedLen(sha1.Size) {
		return "", checksumEncoding{}, false
	}
	decoded, err := decoder.DecodeString(checksum)
	if err != nil || len(decoded) != sha1.Size {
		return "", checksumEncoding{}, false
	}
	return hex.EncodeToString(decoded), checksumEncoding{base64: true, rawBase64: rawBase64}, true
}

func encodeChecksumSHA1(sha1Hex string, encoding checksumEncoding) string {
	decoded, _ := hex.DecodeString(sha1Hex)
	if !encoding.base64 {
		encoded := hex.EncodeToString(decoded)
		if encoding.upperHex {
			return strings.ToUpper(encoded)
		}
		return encoded
	}
	if encoding.rawBase64 {
		return base64.RawStdEncoding.EncodeToString(decoded)
	}
	return base64.StdEncoding.EncodeToString(decoded)
}
