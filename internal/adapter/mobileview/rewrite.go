package mobileview

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Lookup resolves the original SHA-1 for an asset owned by ownerID. The SHA-1
// must be returned as 40 hexadecimal characters.
type Lookup func(ownerID, assetID string) (sourceSHA1Hex string, ok bool)

var syncAssetEventTypes = map[string]struct{}{
	"AssetV2":                {},
	"PartnerAssetV2":         {},
	"PartnerAssetBackfillV2": {},
	"AlbumAssetCreateV2":     {},
	"AlbumAssetUpdateV2":     {},
	"AlbumAssetBackfillV2":   {},
}

// RewriteJSON recursively replaces checksum strings in JSON asset objects.
//
// An asset object must contain string fields named id and checksum. If it has an
// ownerId field, that value must equal ownerID before Lookup is called. Objects
// without ownerId use ownerID as the authenticated owner. A nil Lookup, a
// missing source SHA-1, or an unsupported checksum encoding leaves the object
// unchanged. Invalid JSON and an invalid SHA-1 returned by Lookup are errors.
func RewriteJSON(data []byte, ownerID string, lookup Lookup) ([]byte, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode JSON response: %w", err)
	}

	rewritten, changed, err := rewriteJSONValue(raw, ownerID, lookup, []string{"id"})
	if err != nil {
		return nil, err
	}
	if !changed {
		return data, nil
	}
	return rewritten, nil
}

// RewriteNDJSON streams Immich JSON Lines sync records from src to dst.
//
// Each non-empty line must be one JSON object. Records outside the Immich
// v3.3.0 checksum-bearing asset event type allowlist are copied byte-for-byte.
// For supported asset events, data is visited recursively and may identify its
// asset with either id or assetId. Errors include the one-based line number.
func RewriteNDJSON(dst io.Writer, src io.Reader, ownerID string, lookup Lookup) error {
	reader := bufio.NewReader(src)
	lineNumber := 0

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNumber++
			rewritten, err := rewriteNDJSONLine(line, ownerID, lookup)
			if err != nil {
				return fmt.Errorf("line %d: %w", lineNumber, err)
			}
			if err := writeAll(dst, rewritten); err != nil {
				return fmt.Errorf("write line %d: %w", lineNumber, err)
			}
		}

		if readErr == nil {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		return fmt.Errorf("read line %d: %w", lineNumber+1, readErr)
	}
}

func rewriteNDJSONLine(line []byte, ownerID string, lookup Lookup) ([]byte, error) {
	content, lineEnding := splitLineEnding(line)
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, errors.New("empty JSON record")
	}

	var record map[string]json.RawMessage
	if err := json.Unmarshal(content, &record); err != nil {
		return nil, fmt.Errorf("decode JSON record: %w", err)
	}
	if record == nil {
		return nil, errors.New("JSON record must be an object")
	}

	typeJSON, ok := record["type"]
	if !ok {
		return nil, errors.New("JSON record is missing type")
	}
	var eventType string
	if err := json.Unmarshal(typeJSON, &eventType); err != nil {
		return nil, fmt.Errorf("decode record type: %w", err)
	}
	if _, isAssetEvent := syncAssetEventTypes[eventType]; !isAssetEvent {
		return line, nil
	}

	data, ok := record["data"]
	if !ok {
		return nil, errors.New("asset event is missing data")
	}
	trimmedData := bytes.TrimSpace(data)
	if len(trimmedData) == 0 || trimmedData[0] != '{' {
		return nil, errors.New("asset event data must be an object")
	}

	rewrittenData, changed, err := rewriteJSONValue(data, ownerID, lookup, []string{"id", "assetId"})
	if err != nil {
		return nil, err
	}
	if !changed {
		return line, nil
	}

	record["data"] = rewrittenData
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode JSON record: %w", err)
	}

	rewritten := make([]byte, 0, len(encoded)+len(lineEnding))
	rewritten = append(rewritten, encoded...)
	rewritten = append(rewritten, lineEnding...)
	return rewritten, nil
}

func rewriteJSONValue(
	raw json.RawMessage,
	ownerID string,
	lookup Lookup,
	assetIDKeys []string,
) (json.RawMessage, bool, error) {
	first, ok := firstJSONToken(raw)
	if !ok {
		return raw, false, nil
	}

	switch first {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, false, fmt.Errorf("decode JSON object: %w", err)
		}

		changed := false
		for key, value := range object {
			rewritten, childChanged, err := rewriteJSONValue(value, ownerID, lookup, assetIDKeys)
			if err != nil {
				return nil, false, err
			}
			if childChanged {
				object[key] = rewritten
				changed = true
			}
		}

		assetID, checksum, eligible := readAssetFields(object, assetIDKeys)
		if eligible && ownerMatches(object, ownerID) && lookup != nil {
			sourceSHA1Hex, found := lookup(ownerID, assetID)
			if found {
				replacement, supported, err := encodeSourceChecksum(sourceSHA1Hex, checksum)
				if err != nil {
					return nil, false, fmt.Errorf("asset %q: %w", assetID, err)
				}
				if supported && replacement != checksum {
					encodedChecksum, err := json.Marshal(replacement)
					if err != nil {
						return nil, false, fmt.Errorf("encode asset checksum: %w", err)
					}
					object["checksum"] = encodedChecksum
					changed = true
				}
			}
		}

		if !changed {
			return raw, false, nil
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, false, fmt.Errorf("encode JSON object: %w", err)
		}
		return encoded, true, nil

	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, false, fmt.Errorf("decode JSON array: %w", err)
		}

		changed := false
		for index, value := range values {
			rewritten, childChanged, err := rewriteJSONValue(value, ownerID, lookup, assetIDKeys)
			if err != nil {
				return nil, false, err
			}
			if childChanged {
				values[index] = rewritten
				changed = true
			}
		}
		if !changed {
			return raw, false, nil
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return nil, false, fmt.Errorf("encode JSON array: %w", err)
		}
		return encoded, true, nil

	default:
		return raw, false, nil
	}
}

func readAssetFields(object map[string]json.RawMessage, assetIDKeys []string) (string, string, bool) {
	checksumJSON, hasChecksum := object["checksum"]
	if !hasChecksum {
		return "", "", false
	}
	var checksum string
	if err := json.Unmarshal(checksumJSON, &checksum); err != nil {
		return "", "", false
	}

	for _, key := range assetIDKeys {
		assetIDJSON, present := object[key]
		if !present {
			continue
		}
		var assetID string
		if err := json.Unmarshal(assetIDJSON, &assetID); err != nil || assetID == "" {
			return "", "", false
		}
		return assetID, checksum, true
	}
	return "", "", false
}

func ownerMatches(object map[string]json.RawMessage, ownerID string) bool {
	ownerJSON, present := object["ownerId"]
	if !present {
		return true
	}
	var objectOwnerID string
	if err := json.Unmarshal(ownerJSON, &objectOwnerID); err != nil {
		return false
	}
	return objectOwnerID == ownerID
}

func encodeSourceChecksum(sourceSHA1Hex, original string) (string, bool, error) {
	decoded, err := hex.DecodeString(sourceSHA1Hex)
	if err != nil || len(decoded) != sha1.Size {
		return "", false, fmt.Errorf("lookup returned invalid SHA-1 hex %q", sourceSHA1Hex)
	}

	if isHexSHA1(original) {
		return hex.EncodeToString(decoded), true, nil
	}

	if originalDecoded, err := base64.StdEncoding.DecodeString(original); err == nil && len(originalDecoded) == sha1.Size {
		if strings.Contains(original, "=") {
			return base64.StdEncoding.EncodeToString(decoded), true, nil
		}
		return base64.RawStdEncoding.EncodeToString(decoded), true, nil
	}
	if originalDecoded, err := base64.RawStdEncoding.DecodeString(original); err == nil && len(originalDecoded) == sha1.Size {
		return base64.RawStdEncoding.EncodeToString(decoded), true, nil
	}

	return "", false, nil
}

func isHexSHA1(value string) bool {
	if len(value) != sha1.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha1.Size
}

func firstJSONToken(raw []byte) (byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return 0, false
	}
	return trimmed[0], true
}

func splitLineEnding(line []byte) ([]byte, []byte) {
	if bytes.HasSuffix(line, []byte("\r\n")) {
		return line[:len(line)-2], []byte("\r\n")
	}
	if bytes.HasSuffix(line, []byte("\n")) {
		return line[:len(line)-1], []byte("\n")
	}
	return line, nil
}

func writeAll(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := dst.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
