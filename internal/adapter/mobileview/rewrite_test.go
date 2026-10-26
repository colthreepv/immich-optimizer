package mobileview

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const (
	testOwnerID = "owner-1"
	testAssetID = "asset-1"
	serverSHA1  = "4242424242424242424242424242424242424242"
	sourceSHA1  = "1111111111111111111111111111111111111111"
)

func TestRewriteJSONSingleAsset(t *testing.T) {
	originalChecksum := base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1))
	expectedChecksum := base64.StdEncoding.EncodeToString(mustHex(t, sourceSHA1))
	input := []byte(`{
		"id":"asset-1",
		"ownerId":"owner-1",
		"checksum":"` + originalChecksum + `",
		"originalFileName":"photo.jpg"
	}`)

	var lookupCalls int
	got, err := RewriteJSON(input, testOwnerID, func(ownerID, assetID string) (string, bool) {
		lookupCalls++
		if ownerID != testOwnerID || assetID != testAssetID {
			t.Fatalf("unexpected lookup arguments: %q, %q", ownerID, assetID)
		}
		return sourceSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteJSON returned error: %v", err)
	}
	if lookupCalls != 1 {
		t.Fatalf("lookup called %d times, want 1", lookupCalls)
	}

	var asset map[string]any
	if err := json.Unmarshal(got, &asset); err != nil {
		t.Fatalf("decode rewritten JSON: %v", err)
	}
	if asset["checksum"] != expectedChecksum {
		t.Fatalf("checksum = %v, want %s", asset["checksum"], expectedChecksum)
	}
	if asset["originalFileName"] != "photo.jpg" {
		t.Fatalf("unrelated field changed: %v", asset["originalFileName"])
	}
}

func TestRewriteJSONSearchListsAndNestedAssets(t *testing.T) {
	originalChecksum := base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1))
	expectedChecksum := base64.StdEncoding.EncodeToString(mustHex(t, sourceSHA1))
	input := []byte(`{
		"items":[
			{"id":"asset-1","ownerId":"owner-1","checksum":"` + originalChecksum + `"},
			{"id":"asset-2","ownerId":"owner-1","checksum":"` + originalChecksum + `"}
		],
		"asset":{"wrapper":{"id":"asset-3","ownerId":"owner-1","checksum":"` + originalChecksum + `"}},
		"notAnAsset":{"id":"asset-4","checksum":"` + originalChecksum + `","ownerId":"different-owner"}
	}`)

	got, err := RewriteJSON(input, testOwnerID, func(_, _ string) (string, bool) {
		return sourceSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteJSON returned error: %v", err)
	}

	var response struct {
		Items []struct {
			Checksum string `json:"checksum"`
		} `json:"items"`
		Asset struct {
			Wrapper struct {
				Checksum string `json:"checksum"`
			} `json:"wrapper"`
		} `json:"asset"`
		NotAnAsset struct {
			Checksum string `json:"checksum"`
		} `json:"notAnAsset"`
	}
	if err := json.Unmarshal(got, &response); err != nil {
		t.Fatalf("decode rewritten JSON: %v", err)
	}
	for index, item := range response.Items {
		if item.Checksum != expectedChecksum {
			t.Errorf("item %d checksum = %s, want %s", index, item.Checksum, expectedChecksum)
		}
	}
	if response.Asset.Wrapper.Checksum != expectedChecksum {
		t.Fatalf("nested checksum = %s, want %s", response.Asset.Wrapper.Checksum, expectedChecksum)
	}
	if response.NotAnAsset.Checksum != originalChecksum {
		t.Fatalf("different owner checksum = %s, want unchanged %s", response.NotAnAsset.Checksum, originalChecksum)
	}
}

func TestRewriteJSONPreservesChecksumEncoding(t *testing.T) {
	input := []byte(`[
		{"id":"asset-base64","ownerId":"owner-1","checksum":"` +
		base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1)) + `"},
		{"id":"asset-hex","ownerId":"owner-1","checksum":"` + strings.ToUpper(serverSHA1) + `"}
	]`)
	got, err := RewriteJSON(input, testOwnerID, func(_, _ string) (string, bool) {
		return sourceSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteJSON returned error: %v", err)
	}

	var assets []map[string]any
	if err := json.Unmarshal(got, &assets); err != nil {
		t.Fatalf("decode rewritten JSON: %v", err)
	}
	if got := assets[0]["checksum"]; got != base64.StdEncoding.EncodeToString(mustHex(t, sourceSHA1)) {
		t.Fatalf("base64 checksum = %v", got)
	}
	if got := assets[1]["checksum"]; got != sourceSHA1 {
		t.Fatalf("hex checksum = %v, want %s", got, sourceSHA1)
	}
}

func TestRewriteJSONMissingOwnerUsesAuthenticatedOwner(t *testing.T) {
	originalChecksum := base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1))
	input := []byte(`{"id":"asset-1","checksum":"` + originalChecksum + `"}`)
	got, err := RewriteJSON(input, testOwnerID, func(ownerID, assetID string) (string, bool) {
		if ownerID != testOwnerID || assetID != testAssetID {
			t.Fatalf("unexpected lookup arguments: %q, %q", ownerID, assetID)
		}
		return sourceSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteJSON returned error: %v", err)
	}
	if !bytes.Contains(got, []byte(base64.StdEncoding.EncodeToString(mustHex(t, sourceSHA1)))) {
		t.Fatalf("checksum was not rewritten: %s", got)
	}
}

func TestRewriteNDJSONAssetEventShapes(t *testing.T) {
	assetEventTypes := []string{
		"AssetV2",
		"PartnerAssetV2",
		"PartnerAssetBackfillV2",
		"AlbumAssetCreateV2",
		"AlbumAssetUpdateV2",
		"AlbumAssetBackfillV2",
	}
	originalChecksum := base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1))
	expectedChecksum := base64.StdEncoding.EncodeToString(mustHex(t, sourceSHA1))
	var input strings.Builder
	for index, eventType := range assetEventTypes {
		assetIDKey := "id"
		if index == 0 {
			assetIDKey = "assetId"
		}
		input.WriteString(assetEventLine(
			eventType,
			`"`+assetIDKey+`":"asset-`+eventType+`","ownerId":"owner-1","checksum":"`+originalChecksum+`"`,
			"ack-"+eventType,
		))
	}

	unknownLine := `{ "type":"FutureAssetV9", "data":{"id":"asset-future","ownerId":"owner-1","checksum":"` + originalChecksum + `"}, "extra":true }` + "\n"
	metadataLine := `{"type":"AssetMetadataV1","data":{"assetId":"asset-metadata","ownerId":"owner-1","checksum":"` + originalChecksum + `"},"ack":"metadata"}` + "\n"
	input.WriteString(unknownLine)
	input.WriteString(metadataLine)

	var output bytes.Buffer
	var lookupCalls int
	err := RewriteNDJSON(&output, strings.NewReader(input.String()), testOwnerID, func(ownerID, assetID string) (string, bool) {
		lookupCalls++
		if ownerID != testOwnerID || !strings.HasPrefix(assetID, "asset-") {
			t.Fatalf("unexpected lookup arguments: %q, %q", ownerID, assetID)
		}
		return sourceSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteNDJSON returned error: %v", err)
	}
	if lookupCalls != len(assetEventTypes) {
		t.Fatalf("lookup called %d times, want %d", lookupCalls, len(assetEventTypes))
	}

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != len(assetEventTypes)+2 {
		t.Fatalf("got %d output lines, want %d", len(lines), len(assetEventTypes)+2)
	}
	for index, eventType := range assetEventTypes {
		var record struct {
			Type string `json:"type"`
			Ack  string `json:"ack"`
			Data struct {
				Checksum string `json:"checksum"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(lines[index]), &record); err != nil {
			t.Fatalf("decode output line %d: %v", index+1, err)
		}
		if record.Type != eventType || record.Ack != "ack-"+eventType {
			t.Errorf("line %d changed event envelope: %+v", index+1, record)
		}
		if record.Data.Checksum != expectedChecksum {
			t.Errorf("line %d checksum = %s, want %s", index+1, record.Data.Checksum, expectedChecksum)
		}
	}
	if lines[len(assetEventTypes)]+"\n" != unknownLine {
		t.Fatalf("unknown record was not preserved byte-for-byte")
	}
	if lines[len(assetEventTypes)+1]+"\n" != metadataLine {
		t.Fatalf("metadata record was not preserved byte-for-byte")
	}
}

func TestRewriteNDJSONOtherOwnerUntouched(t *testing.T) {
	originalChecksum := base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1))
	line := `{"type":"AssetV2","data":{"id":"asset-1","ownerId":"owner-2","checksum":"` + originalChecksum + `"},"ack":"1"}` + "\n"
	var output bytes.Buffer
	lookupCalls := 0

	err := RewriteNDJSON(&output, strings.NewReader(line), testOwnerID, func(_, _ string) (string, bool) {
		lookupCalls++
		return sourceSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteNDJSON returned error: %v", err)
	}
	if lookupCalls != 0 {
		t.Fatalf("lookup called %d times for another owner", lookupCalls)
	}
	if output.String() != line {
		t.Fatalf("other-owner record changed\nwant: %s\ngot:  %s", line, output.String())
	}
}

func TestRewriteNDJSONMalformedAndTruncatedRecords(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "malformed", input: `{"type":"AssetV2","data":}` + "\n"},
		{name: "truncated", input: `{"type":"AssetV2","data":{"id":"asset-1"`},
		{name: "blank record", input: "\n"},
		{name: "non-object", input: `[]` + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := RewriteNDJSON(io.Discard, strings.NewReader(test.input), testOwnerID, func(_, _ string) (string, bool) {
				return sourceSHA1, true
			})
			if err == nil {
				t.Fatal("RewriteNDJSON returned nil error")
			}
			if !strings.Contains(err.Error(), "line 1") {
				t.Fatalf("error %q does not identify line 1", err)
			}
		})
	}
}

func TestRewriteJSONRejectsInvalidLookupSHA1(t *testing.T) {
	originalChecksum := base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1))
	input := []byte(`{"id":"asset-1","ownerId":"owner-1","checksum":"` + originalChecksum + `"}`)
	_, err := RewriteJSON(input, testOwnerID, func(_, _ string) (string, bool) {
		return "not-a-sha1", true
	})
	if err == nil {
		t.Fatal("RewriteJSON returned nil error for invalid lookup SHA-1")
	}
}

func assetEventLine(eventType, data, ack string) string {
	return `{"type":"` + eventType + `","data":{` + data + `},"ack":"` + ack + `"}` + "\n"
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode test hex: %v", err)
	}
	return decoded
}
