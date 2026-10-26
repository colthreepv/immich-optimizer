package mobileview

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestRewriteBulkUploadCheckRequestSingleBase64(t *testing.T) {
	sourceChecksum := base64.StdEncoding.EncodeToString(mustHex(t, sourceSHA1))
	expectedChecksum := base64.StdEncoding.EncodeToString(mustHex(t, serverSHA1))
	input := []byte(`{"assets":[{"id":"local-photo.jpg","checksum":"` + sourceChecksum + `","clientExtra":7}]}`)
	var calls int

	got, err := RewriteBulkUploadCheckRequest(input, testOwnerID, func(ownerID, source string) (string, bool) {
		calls++
		if ownerID != testOwnerID || source != sourceSHA1 {
			t.Fatalf("lookup(%q, %q), want (%q, %q)", ownerID, source, testOwnerID, sourceSHA1)
		}
		return serverSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteBulkUploadCheckRequest() error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", calls)
	}

	var request struct {
		Assets []struct {
			ID          string `json:"id"`
			Checksum    string `json:"checksum"`
			ClientExtra int    `json:"clientExtra"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(got, &request); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(request.Assets) != 1 {
		t.Fatalf("assets = %d, want 1", len(request.Assets))
	}
	if request.Assets[0].ID != "local-photo.jpg" || request.Assets[0].ClientExtra != 7 {
		t.Fatalf("unrelated item fields changed: %+v", request.Assets[0])
	}
	if request.Assets[0].Checksum != expectedChecksum {
		t.Fatalf("checksum = %q, want %q", request.Assets[0].Checksum, expectedChecksum)
	}
}

func TestRewriteBulkUploadCheckRequestPreservesListOrderAndEncodings(t *testing.T) {
	sourceBytes := mustHex(t, sourceSHA1)
	const outputSHA1 = "abcdef0123456789abcdef0123456789abcdef01"
	outputBytes := mustHex(t, outputSHA1)
	tests := []struct {
		name       string
		input      string
		wantOutput string
	}{
		{
			name:       "padded base64",
			input:      base64.StdEncoding.EncodeToString(sourceBytes),
			wantOutput: base64.StdEncoding.EncodeToString(outputBytes),
		},
		{
			name:       "raw base64",
			input:      base64.RawStdEncoding.EncodeToString(sourceBytes),
			wantOutput: base64.RawStdEncoding.EncodeToString(outputBytes),
		},
		{
			name:       "lowercase hex",
			input:      sourceSHA1,
			wantOutput: outputSHA1,
		},
		{
			name:       "uppercase hex",
			input:      strings.ToUpper(outputSHA1),
			wantOutput: strings.ToUpper(outputSHA1),
		},
	}
	inputChecksums := make([]string, 0, len(tests)+1)
	wantChecksums := make([]string, 0, len(tests)+1)
	var body strings.Builder
	body.WriteString(`{"assets":[`)
	for index, test := range tests {
		if index != 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"id":"id-` + test.name + `","checksum":"` + test.input + `"}`)
		inputChecksums = append(inputChecksums, test.input)
		wantChecksums = append(wantChecksums, test.wantOutput)
	}
	const unmappedChecksum = "3333333333333333333333333333333333333333"
	body.WriteString(`,{"id":"unmapped","checksum":"` + unmappedChecksum + `"}]}`)
	inputChecksums = append(inputChecksums, unmappedChecksum)
	wantChecksums = append(wantChecksums, unmappedChecksum)

	var calls []string
	got, err := RewriteBulkUploadCheckRequest([]byte(body.String()), testOwnerID, func(ownerID, source string) (string, bool) {
		calls = append(calls, source)
		if source == unmappedChecksum {
			return "", false
		}
		return outputSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteBulkUploadCheckRequest() error = %v", err)
	}
	if len(calls) != len(tests)+1 {
		t.Fatalf("lookup calls = %d, want %d", len(calls), len(tests)+1)
	}

	var request struct {
		Assets []struct {
			ID       string `json:"id"`
			Checksum string `json:"checksum"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(got, &request); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(request.Assets) != len(wantChecksums) {
		t.Fatalf("assets = %d, want %d", len(request.Assets), len(wantChecksums))
	}
	for index := range tests {
		wantID := "id-" + tests[index].name
		if request.Assets[index].ID != wantID {
			t.Errorf("asset %d id = %q, want %q", index, request.Assets[index].ID, wantID)
		}
		if request.Assets[index].Checksum != wantChecksums[index] {
			t.Errorf("asset %d checksum = %q, want %q", index, request.Assets[index].Checksum, wantChecksums[index])
		}
	}
	if request.Assets[len(tests)].ID != "unmapped" || request.Assets[len(tests)].Checksum != unmappedChecksum {
		t.Fatalf("unmapped item changed: %+v", request.Assets[len(tests)])
	}
}

func TestRewriteBulkUploadCheckRequestNoMappingReturnsInput(t *testing.T) {
	input := []byte("{\n  \"assets\": [{\"id\":\"photo.jpg\",\"checksum\":\"" + sourceSHA1 + "\",\"unrelated\":true}]\n}\n")
	got, err := RewriteBulkUploadCheckRequest(input, testOwnerID, func(_, _ string) (string, bool) {
		return "", false
	})
	if err != nil {
		t.Fatalf("RewriteBulkUploadCheckRequest() error = %v", err)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("unmapped request bytes changed\nwant: %s\ngot:  %s", input, got)
	}
}

func TestRewriteBulkUploadCheckRequestRejectsMalformedInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "truncated", input: `{"assets":[{"id":"photo.jpg","checksum":"`},
		{name: "root array", input: `[]`},
		{name: "missing assets", input: `{"other":[]}`},
		{name: "assets object", input: `{"assets":{}}`},
		{name: "asset scalar", input: `{"assets":["photo"]}`},
		{name: "missing id", input: `{"assets":[{"checksum":"` + sourceSHA1 + `"}]}`},
		{name: "missing checksum", input: `{"assets":[{"id":"photo.jpg"}]}`},
		{name: "checksum number", input: `{"assets":[{"id":"photo.jpg","checksum":1}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RewriteBulkUploadCheckRequest([]byte(test.input), testOwnerID, nil); err == nil {
				t.Fatal("RewriteBulkUploadCheckRequest() returned nil error")
			}
		})
	}
}

func TestRewriteBulkUploadCheckRequestRejectsInvalidLookupSHA1(t *testing.T) {
	input := []byte(`{"assets":[{"id":"photo.jpg","checksum":"` + sourceSHA1 + `"}]}`)
	_, err := RewriteBulkUploadCheckRequest(input, testOwnerID, func(_, _ string) (string, bool) {
		return "not-a-sha1", true
	})
	if err == nil || !strings.Contains(err.Error(), "invalid output SHA-1") {
		t.Fatalf("error = %v, want invalid output SHA-1", err)
	}
}

func TestParseBulkUploadCheckResponsePreservesIDsActionsAndReasons(t *testing.T) {
	input := []byte(`{
		"results":[
			{"id":"photos/a.jpg","action":"reject","reason":"duplicate","assetId":"asset-a","isTrashed":false},
			{"id":"photos/b.jpg","action":"accept","futureField":{"kept":true}},
			{"id":"photos/c.jpg","action":"reject","reason":"unsupported-format","isTrashed":true}
		]
	}`)

	results, err := ParseBulkUploadCheckResponse(input)
	if err != nil {
		t.Fatalf("ParseBulkUploadCheckResponse() error = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	if results[0].ID != "photos/a.jpg" || results[0].Action != "reject" ||
		results[0].Reason != "duplicate" || results[0].AssetID != "asset-a" ||
		results[0].IsTrashed == nil || *results[0].IsTrashed {
		t.Fatalf("first result changed: %+v", results[0])
	}
	if results[1].ID != "photos/b.jpg" || results[1].Action != "accept" || results[1].Reason != "" {
		t.Fatalf("second result changed: %+v", results[1])
	}
	if results[2].ID != "photos/c.jpg" || results[2].Reason != "unsupported-format" ||
		results[2].IsTrashed == nil || !*results[2].IsTrashed {
		t.Fatalf("third result changed: %+v", results[2])
	}
}

func TestParseBulkUploadCheckResponseRejectsMalformedResults(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "truncated", input: `{"results":[{"id":"a.jpg","action":"accept"}`},
		{name: "missing results", input: `{}`},
		{name: "results object", input: `{"results":{}}`},
		{name: "missing id", input: `{"results":[{"action":"accept"}]}`},
		{name: "missing action", input: `{"results":[{"id":"a.jpg"}]}`},
		{name: "unknown action", input: `{"results":[{"id":"a.jpg","action":"maybe"}]}`},
		{name: "unknown reason", input: `{"results":[{"id":"a.jpg","action":"reject","reason":"other"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseBulkUploadCheckResponse([]byte(test.input)); err == nil {
				t.Fatal("ParseBulkUploadCheckResponse() returned nil error")
			}
		})
	}
}

func TestBulkUploadCheckRequestDoesNotTreatOtherChecksumsAsAssets(t *testing.T) {
	checksum := base64.StdEncoding.EncodeToString(mustHex(t, sourceSHA1))
	input := []byte(`{"assets":[{"id":"a.jpg","checksum":"` + checksum + `"}],"checksum":"` + checksum + `","nested":{"id":"x","checksum":"` + checksum + `"}}`)
	got, err := RewriteBulkUploadCheckRequest(input, testOwnerID, func(_, _ string) (string, bool) {
		return serverSHA1, true
	})
	if err != nil {
		t.Fatalf("RewriteBulkUploadCheckRequest() error = %v", err)
	}
	var request struct {
		Assets []struct {
			Checksum string `json:"checksum"`
		} `json:"assets"`
		Checksum string `json:"checksum"`
		Nested   struct {
			Checksum string `json:"checksum"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(got, &request); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if request.Checksum != checksum || request.Nested.Checksum != checksum {
		t.Fatal("rewrote checksum outside the exact assets array")
	}
	if request.Assets[0].Checksum == checksum {
		t.Fatal("did not rewrite the exact request item")
	}
}

func TestDecodeChecksumSHA1RejectsWrongLengthOrInvalidEncoding(t *testing.T) {
	if _, _, ok := decodeChecksumSHA1(hex.EncodeToString([]byte("short"))); ok {
		t.Fatal("accepted a short hex checksum")
	}
	if _, _, ok := decodeChecksumSHA1("z" + sourceSHA1[1:]); ok {
		t.Fatal("accepted invalid hex")
	}
	if _, _, ok := decodeChecksumSHA1(strings.Repeat("!", 28)); ok {
		t.Fatal("accepted invalid base64")
	}
}
