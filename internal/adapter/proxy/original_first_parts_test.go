package proxy

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestOriginalFirstPartsPreserveMultipart(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("metadata", `[{"key":"description","value":"hello"}]`)
	_ = mw.WriteField("tag", "first")
	_ = mw.WriteField("tag", "second")
	source, _ := mw.CreateFormFile("assetData", "photo.jpg")
	_, _ = source.Write([]byte("original image"))
	sidecar, _ := mw.CreateFormFile("sidecarData", "photo.xmp")
	_, _ = sidecar.Write([]byte("sidecar content"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	parts, err := captureOriginalFirstParts(req, t.TempDir(), "assetData")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha1.Sum([]byte("original image"))
	if parts.SourceFilename != "photo.jpg" || parts.SourceSize != 14 || parts.SourceSHA1 != hex.EncodeToString(hash[:]) {
		t.Fatalf("source = %#v", parts)
	}
	var encoded bytes.Buffer
	out := multipart.NewWriter(&encoded)
	if err := writeOriginalFirstParts(out, parts.Parts); err != nil {
		t.Fatal(err)
	}
	reader := multipart.NewReader(&encoded, out.Boundary())
	var names, contents []string
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(p)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, p.FormName()+":"+p.FileName())
		contents = append(contents, string(data))
	}
	if !reflect.DeepEqual(names, []string{"metadata:", "tag:", "tag:", "assetData:photo.jpg", "sidecarData:photo.xmp"}) {
		t.Fatalf("names: %v", names)
	}
	if !reflect.DeepEqual(contents, []string{`[{"key":"description","value":"hello"}]`, "first", "second", "original image", "sidecar content"}) {
		t.Fatalf("contents: %v", contents)
	}
}

func TestOriginalFirstPartsRejectsDuplicateSource(t *testing.T) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_, _ = mw.CreateFormFile("assetData", "one.jpg")
	_, _ = mw.CreateFormFile("assetData", "two.jpg")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/assets", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if _, err := captureOriginalFirstParts(req, t.TempDir(), "assetData"); err == nil {
		t.Fatal("accepted duplicate source")
	}
}
