// Package replacement publishes a new Immich asset without modifying the original's bytes.
package replacement

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
)

// ErrBlocked indicates that replacement cannot be proven safe and requires review.
var ErrBlocked = errors.New("replacement blocked")

// Input identifies an owner-scoped original and a complete, immutable output file.
// Hashes are lowercase or uppercase hexadecimal digests. OutputAssetID is a durable
// checkpoint from an earlier attempt, if any. SourceSHA256 may be empty.
type Input struct {
	OwnerID, OriginalAssetID, SourceSHA1, SourceSHA256                  string
	InputName, OutputName, Preset, OutputPath, OutputSHA1, OutputSHA256 string
	SourceSize, OutputSize                                              int64
	OutputAssetID                                                       string
	// VerifyAVIF must independently verify that the output preserves the original's
	// relevant image metadata and color profile. It must return an error if uncertain.
	VerifyAVIF func(context.Context, string, string) error
	// AuditRelations must reject comments, manual faces and other unsupported relationships.
	AuditRelations func(context.Context, string, string) error
}

// Result is a verified mapping. Persist it atomically before Retire.
type Result struct {
	OwnerID, OriginalAssetID, OutputAssetID, SourceSHA1, OutputSHA1, OutputSHA256 string
	SourceSize, OutputSize                                                        *int64
}

// Checkpoint must durably persist the output asset ID before returning nil.
type Stage string

const (
	StageUploaded         Stage = "uploaded"
	StageVerified         Stage = "verified"
	StageMetadataMigrated Stage = "metadata_migrated"
)

type Checkpoint func(context.Context, Stage, Result) error

type Client struct {
	base, key string
	http      *http.Client
}

func NewClient(serverURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	base := strings.TrimRight(serverURL, "/")
	if !strings.HasSuffix(base, "/api") {
		base += "/api"
	}
	return &Client{base: base, key: apiKey, http: hc}
}

type asset struct {
	Width            *int64            `json:"width"`
	Height           *int64            `json:"height"`
	ID               string            `json:"id"`
	OwnerID          string            `json:"ownerId"`
	Checksum         string            `json:"checksum"`
	IsTrashed        bool              `json:"isTrashed"`
	IsOffline        bool              `json:"isOffline"`
	IsEdited         bool              `json:"isEdited"`
	IsFavorite       bool              `json:"isFavorite"`
	Visibility       string            `json:"visibility"`
	Type             string            `json:"type"`
	LivePhotoVideoID *string           `json:"livePhotoVideoId"`
	Stack            json.RawMessage   `json:"stack"`
	FileCreatedAt    string            `json:"fileCreatedAt"`
	FileModifiedAt   string            `json:"fileModifiedAt"`
	Exif             map[string]any    `json:"exifInfo"`
	People           []json.RawMessage `json:"people"`
	Tags             []json.RawMessage `json:"tags"`
}
type album struct {
	ID    string  `json:"id"`
	Cover *string `json:"albumThumbnailAssetId"`
}
type metadata struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

const provenanceKey = "storage-saver-source"

// Immich metadata fields that are copied and checked together.
var migratedExifFields = [...]string{"description", "dateTimeOriginal", "latitude", "longitude", "rating", "timeZone"}

func blocked(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBlocked, fmt.Sprintf(format, a...))
}
func (c *Client) request(ctx context.Context, method, path, contentType string, body io.Reader, dst any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("immich %s %s: status %d: %s", method, path, res.StatusCode, string(b))
	}
	if dst != nil {
		if err = json.NewDecoder(res.Body).Decode(dst); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}
func (c *Client) json(ctx context.Context, method, path string, body, dst any) error {
	var r io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		r = bytes.NewReader(b)
	}
	return c.request(ctx, method, path, "application/json", r, dst)
}
func pathID(id string) string { return url.PathEscape(id) }
func hexDigest(s string, n int) ([]byte, error) {
	b, e := hex.DecodeString(s)
	if e != nil || len(b) != n {
		return nil, blocked("invalid digest")
	}
	return b, nil
}
func hashes(path string) (string, string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", "", e
	}
	defer f.Close()
	a, b := sha1.New(), sha256.New()
	if _, e = io.Copy(io.MultiWriter(a, b), f); e != nil {
		return "", "", e
	}
	return hex.EncodeToString(a.Sum(nil)), hex.EncodeToString(b.Sum(nil)), nil
}
func (c *Client) getAsset(ctx context.Context, id string) (asset, error) {
	var a asset
	e := c.json(ctx, "GET", "/assets/"+pathID(id), nil, &a)
	return a, e
}
func checksum(a asset, want string) bool {
	b, e := base64.StdEncoding.DecodeString(a.Checksum)
	w, e2 := hexDigest(want, sha1.Size)
	return e == nil && e2 == nil && bytes.Equal(b, w)
}
func guard(a asset, in Input) error {
	if a.ID != in.OriginalAssetID || a.OwnerID != in.OwnerID || a.IsTrashed || a.IsOffline || !checksum(a, in.SourceSHA1) {
		return blocked("original missing, inactive, changed, or wrong owner")
	}
	if a.IsEdited || a.Visibility == "locked" || a.LivePhotoVideoID != nil || (len(a.Stack) > 0 && string(a.Stack) != "null") {
		return blocked("edited, locked, live-photo, or stacked original")
	}
	if a.Visibility != "timeline" && a.Visibility != "archive" {
		return blocked("unsupported visibility %q", a.Visibility)
	}
	if a.Type != "IMAGE" {
		return blocked("only still images are supported")
	}
	if len(a.People) > 0 {
		return blocked("person relationships cannot be proven migrated")
	}
	return nil
}

// metadataReady is a retryable readiness gate for Immich's asynchronous
// initial extraction. Do not classify this as ErrBlocked: extraction may finish.
func metadataReady(a asset, expectedSize int64) error {
	size, ok := a.Exif["fileSizeInByte"].(float64)
	if !ok || size != float64(expectedSize) || a.Width == nil || *a.Width <= 0 || a.Height == nil || *a.Height <= 0 {
		return fmt.Errorf("asset %s metadata extraction not ready", a.ID)
	}
	return nil
}
func (c *Client) original(ctx context.Context, in Input) (asset, error) {
	a, e := c.getAsset(ctx, in.OriginalAssetID)
	if e != nil {
		return a, blocked("cannot read active original: %v", e)
	}
	if e = guard(a, in); e != nil {
		return a, e
	}
	if e = metadataReady(a, in.SourceSize); e != nil {
		return a, e
	}
	sha1sum, sha256sum, size, e := c.downloadHashes(ctx, in.OriginalAssetID)
	if e != nil {
		return a, blocked("cannot verify original bytes: %v", e)
	}
	if size != in.SourceSize || !strings.EqualFold(sha1sum, in.SourceSHA1) || (in.SourceSHA256 != "" && !strings.EqualFold(sha256sum, in.SourceSHA256)) {
		return a, blocked("original bytes changed")
	}

	var faces []json.RawMessage
	if e = c.json(ctx, "GET", "/faces?id="+url.QueryEscape(in.OriginalAssetID), nil, &faces); e != nil {
		return a, blocked("cannot inspect faces: %v", e)
	}
	if faces == nil {
		return a, blocked("invalid face response")
	}
	for _, raw := range faces {
		var face struct {
			SourceType string          `json:"sourceType"`
			Person     json.RawMessage `json:"person"`
			PersonID   json.RawMessage `json:"personId"`
		}
		if e = json.Unmarshal(raw, &face); e != nil || face.SourceType != "machine-learning" || string(face.Person) != "null" || (len(face.PersonID) != 0 && string(face.PersonID) != "null") {
			return a, blocked("face is manual, assigned, or cannot be classified")
		}
	}
	albums, e := c.albums(ctx, in.OriginalAssetID)
	if e != nil {
		return a, blocked("cannot inspect albums: %v", e)
	}
	for _, album := range albums {
		var activities []json.RawMessage
		path := "/activities?albumId=" + url.QueryEscape(album.ID) + "&assetId=" + url.QueryEscape(in.OriginalAssetID)
		if e = c.json(ctx, "GET", path, nil, &activities); e != nil {
			return a, blocked("cannot inspect album activities: %v", e)
		}
		if len(activities) > 0 {
			return a, blocked("album %s contains original activity", album.ID)
		}
	}
	return a, nil
}
func (c *Client) albums(ctx context.Context, id string) ([]album, error) {
	var a []album
	e := c.json(ctx, "GET", "/albums?assetId="+url.QueryEscape(id), nil, &a)
	return a, e
}
func albumSet(a []album) map[string]bool {
	m := map[string]bool{}
	for _, v := range a {
		m[v.ID] = true
	}
	return m
}
func provenance(in Input) map[string]any {
	return map[string]any{"schemaVersion": float64(1), "ownerId": in.OwnerID, "sourceSHA1": strings.ToLower(in.SourceSHA1), "outputSHA1": strings.ToLower(in.OutputSHA1), "sourceFilename": in.InputName, "outputFilename": in.OutputName, "preset": in.Preset, "sourceSize": float64(in.SourceSize), "outputSize": float64(in.OutputSize)}
}
func (c *Client) proveProvenance(ctx context.Context, id string, in Input) error {
	var m metadata
	if e := c.json(ctx, "GET", "/assets/"+pathID(id)+"/metadata/"+provenanceKey, nil, &m); e != nil {
		return blocked("output provenance unavailable: %v", e)
	}
	if !reflect.DeepEqual(m.Value, provenance(in)) {
		return blocked("output provenance mismatch")
	}
	return nil
}
func (c *Client) duplicate(ctx context.Context, in Input) (string, error) {
	var r struct {
		Results []struct {
			ID, Action, AssetID, Reason string
			IsTrashed                   bool
		} `json:"results"`
	}
	body := map[string]any{"assets": []any{map[string]any{"id": "output", "checksum": in.OutputSHA1}}}
	if e := c.json(ctx, "POST", "/assets/bulk-upload-check", body, &r); e != nil {
		return "", e
	}
	if len(r.Results) != 1 || r.Results[0].ID != "output" {
		return "", blocked("invalid duplicate-check response")
	}
	x := r.Results[0]
	if x.Action == "accept" {
		return "", nil
	}
	if x.Action != "reject" || x.Reason != "duplicate" || x.AssetID == "" || x.IsTrashed {
		return "", blocked("output unavailable or duplicate is trashed")
	}
	if e := c.proveProvenance(ctx, x.AssetID, in); e != nil {
		return "", e
	}
	return x.AssetID, nil
}
func (c *Client) upload(ctx context.Context, in Input, a asset) (string, error) {
	// Buffering is deliberately avoided: files can be very large.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var e error
		defer func() {
			if e != nil {
				_ = pw.CloseWithError(e)
			} else {
				_ = pw.Close()
			}
		}()
		fields := map[string]string{"fileCreatedAt": a.FileCreatedAt, "fileModifiedAt": a.FileModifiedAt, "filename": in.OutputName}
		meta, _ := json.Marshal([]any{map[string]any{"key": provenanceKey, "value": provenance(in)}})
		fields["metadata"] = string(meta)
		for k, v := range fields {
			if e = mw.WriteField(k, v); e != nil {
				return
			}
		}
		var p io.Writer
		p, e = mw.CreateFormFile("assetData", in.OutputName)
		if e != nil {
			return
		}
		var f *os.File
		f, e = os.Open(in.OutputPath)
		if e != nil {
			return
		}
		defer f.Close()
		_, e = io.Copy(p, f)
		if e != nil {
			return
		}
		e = mw.Close()
	}()
	var r struct{ ID, Status string }
	e := c.request(ctx, "POST", "/assets", mw.FormDataContentType(), pr, &r)
	_ = pr.Close()
	if e != nil {
		return "", e
	}
	if r.ID == "" {
		return "", blocked("upload returned no asset ID")
	}
	return r.ID, nil
}
func (c *Client) downloadHashes(ctx context.Context, id string) (string, string, int64, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", c.base+"/assets/"+pathID(id)+"/original", nil)
	if e != nil {
		return "", "", 0, e
	}
	req.Header.Set("x-api-key", c.key)
	res, e := c.http.Do(req)
	if e != nil {
		return "", "", 0, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", "", 0, fmt.Errorf("download status %d", res.StatusCode)
	}
	a, b := sha1.New(), sha256.New()
	n, e := io.Copy(io.MultiWriter(a, b), res.Body)
	if e != nil {
		return "", "", 0, e
	}
	return hex.EncodeToString(a.Sum(nil)), hex.EncodeToString(b.Sum(nil)), n, nil
}
func (c *Client) output(ctx context.Context, in Input, id string) error {
	if id == in.OriginalAssetID || id == "" {
		return blocked("output must be a new asset")
	}
	a, e := c.getAsset(ctx, id)
	if e != nil {
		return e
	}
	if a.ID != id || a.OwnerID != in.OwnerID || a.IsTrashed || a.IsOffline || !checksum(a, in.OutputSHA1) {
		return blocked("output owner, state or checksum mismatch")
	}
	if e = c.proveProvenance(ctx, id, in); e != nil {
		return e
	}
	if e = metadataReady(a, in.OutputSize); e != nil {
		return e
	}
	h, h256, size, e := c.downloadHashes(ctx, id)
	if e != nil {
		return e
	}
	if size != in.OutputSize || !strings.EqualFold(h, in.OutputSHA1) || !strings.EqualFold(h256, in.OutputSHA256) {
		return blocked("downloaded output checksum mismatch")
	}
	return nil
}

// Republish emits a fresh Immich sync update after the caller commits the
// source-to-output alias. The current favorite value is written unchanged.
func (c *Client) Republish(ctx context.Context, owner, id string) error {
	if owner == "" || id == "" {
		return blocked("missing output owner or asset ID")
	}
	var a struct {
		ID         string `json:"id"`
		OwnerID    string `json:"ownerId"`
		IsTrashed  *bool  `json:"isTrashed"`
		IsOffline  *bool  `json:"isOffline"`
		IsFavorite *bool  `json:"isFavorite"`
	}
	if e := c.json(ctx, "GET", "/assets/"+pathID(id), nil, &a); e != nil {
		return e
	}
	if a.ID != id || a.OwnerID != owner || a.IsTrashed == nil || *a.IsTrashed || a.IsOffline == nil || *a.IsOffline || a.IsFavorite == nil {
		return blocked("output missing, inactive, wrong owner or favorite unavailable")
	}
	return c.json(ctx, "PUT", "/assets/"+pathID(id), map[string]any{"isFavorite": *a.IsFavorite}, nil)
}

// CanReleaseOriginal checks the native Immich asset before a skipped or failed
// job releases its source spool. It does not audit relationships or download bytes.
func (c *Client) CanReleaseOriginal(ctx context.Context, owner, assetID, sourceSHA1 string) error {
	if owner == "" || assetID == "" {
		return blocked("missing original owner or asset ID")
	}
	if _, e := hexDigest(sourceSHA1, sha1.Size); e != nil {
		return e
	}
	a, e := c.getAsset(ctx, assetID)
	if e != nil {
		return e
	}
	if a.ID != assetID || a.OwnerID != owner || a.IsTrashed || a.IsOffline || !checksum(a, sourceSHA1) {
		return blocked("original missing, inactive, changed, or wrong owner")
	}
	return nil
}

// CanReleaseOutput verifies a complete job's output without requiring staged
// files or migration callbacks. Input.OutputAssetID must contain the durable ID.
func (c *Client) CanReleaseOutput(ctx context.Context, in Input) error {
	if in.OwnerID == "" || in.OriginalAssetID == "" || in.OutputAssetID == "" || in.InputName == "" || in.OutputName == "" || in.Preset == "" || in.SourceSize <= 0 || in.OutputSize <= 0 {
		return blocked("missing output verification input")
	}
	if _, e := hexDigest(in.SourceSHA1, sha1.Size); e != nil {
		return e
	}
	if _, e := hexDigest(in.OutputSHA1, sha1.Size); e != nil {
		return e
	}
	if _, e := hexDigest(in.OutputSHA256, sha256.Size); e != nil {
		return e
	}
	return c.output(ctx, in, in.OutputAssetID)
}

func (c *Client) validateInput(ctx context.Context, in Input) error {
	if in.OwnerID == "" || in.OriginalAssetID == "" || in.OutputPath == "" || in.InputName == "" || in.OutputName == "" || in.Preset == "" || in.SourceSize <= 0 || in.OutputSize <= 0 {
		return blocked("missing required input")
	}
	if _, e := hexDigest(in.SourceSHA1, sha1.Size); e != nil {
		return e
	}
	if in.SourceSHA256 != "" {
		if _, e := hexDigest(in.SourceSHA256, sha256.Size); e != nil {
			return e
		}
	}
	if _, e := hexDigest(in.OutputSHA1, sha1.Size); e != nil {
		return e
	}
	if _, e := hexDigest(in.OutputSHA256, sha256.Size); e != nil {
		return e
	}
	if strings.EqualFold(in.SourceSHA1, in.OutputSHA1) {
		return blocked("output matches original")
	}
	stat, e := os.Stat(in.OutputPath)
	if e != nil {
		return e
	}
	if stat.Size() != in.OutputSize {
		return blocked("output size mismatch")
	}
	h1, h2, e := hashes(in.OutputPath)
	if e != nil {
		return e
	}
	if !strings.EqualFold(h1, in.OutputSHA1) || !strings.EqualFold(h2, in.OutputSHA256) {
		return blocked("local output hash mismatch")
	}
	if in.VerifyAVIF == nil {
		return blocked("AVIF metadata and color verification required")
	}
	if e = in.VerifyAVIF(ctx, in.OriginalAssetID, in.OutputPath); e != nil {
		return blocked("AVIF metadata/color verification: %v", e)
	}
	if in.AuditRelations != nil {
		if e = in.AuditRelations(ctx, in.OwnerID, in.OriginalAssetID); e != nil {
			return blocked("relationship audit: %v", e)
		}
	}
	return nil
}
func result(in Input, id string) Result {
	sourceSize, outputSize := in.SourceSize, in.OutputSize
	return Result{OwnerID: in.OwnerID, OriginalAssetID: in.OriginalAssetID, OutputAssetID: id, SourceSHA1: strings.ToLower(in.SourceSHA1), OutputSHA1: strings.ToLower(in.OutputSHA1), OutputSHA256: strings.ToLower(in.OutputSHA256), SourceSize: &sourceSize, OutputSize: &outputSize}
}

// customMetadata returns all asset metadata, rejecting malformed or ambiguous entries.
func (c *Client) customMetadata(ctx context.Context, id string) (map[string]map[string]any, error) {
	var items []metadata
	if e := c.json(ctx, "GET", "/assets/"+pathID(id)+"/metadata", nil, &items); e != nil {
		return nil, e
	}
	result := make(map[string]map[string]any, len(items))
	for _, item := range items {
		if item.Key == "" || item.Value == nil {
			return nil, blocked("invalid custom metadata")
		}
		if _, ok := result[item.Key]; ok {
			return nil, blocked("duplicate custom metadata key %q", item.Key)
		}
		result[item.Key] = item.Value
	}
	return result, nil
}
func (c *Client) migrateCustomMetadata(ctx context.Context, sourceID, targetID string) error {
	source, e := c.customMetadata(ctx, sourceID)
	if e != nil {
		return e
	}
	target, e := c.customMetadata(ctx, targetID)
	if e != nil {
		return e
	}
	var items []metadata
	for key, value := range source {
		if key == provenanceKey {
			continue
		}
		if old, exists := target[key]; exists {
			if !reflect.DeepEqual(old, value) {
				return blocked("conflicting target custom metadata %q", key)
			}
			continue
		}
		items = append(items, metadata{Key: key, Value: value})
	}
	if len(items) > 0 {
		var ignored []metadata
		if e = c.json(ctx, "PUT", "/assets/"+pathID(targetID)+"/metadata", map[string]any{"items": items}, &ignored); e != nil {
			return e
		}
	}
	return c.verifyCustomMetadata(ctx, sourceID, targetID)
}
func (c *Client) verifyCustomMetadata(ctx context.Context, sourceID, targetID string) error {
	source, e := c.customMetadata(ctx, sourceID)
	if e != nil {
		return e
	}
	target, e := c.customMetadata(ctx, targetID)
	if e != nil {
		return e
	}
	for key, value := range source {
		if key == provenanceKey {
			continue
		}
		if actual, ok := target[key]; !ok || !reflect.DeepEqual(value, actual) {
			return blocked("custom metadata mismatch %q", key)
		}
	}
	return nil
}

// Immich 3.3.5's list endpoint returns at most one preview asset per link.
// Only the detail endpoint returns the full active, EXIF-ready asset set.
type sharedLink struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Assets []struct {
		ID string `json:"id"`
	} `json:"assets"`
}

func (c *Client) sharedLinkDetail(ctx context.Context, id string) (sharedLink, error) {
	var link sharedLink
	if e := c.json(ctx, "GET", "/shared-links/"+pathID(id), nil, &link); e != nil {
		return link, e
	}
	if link.ID != id || link.Type != "INDIVIDUAL" || link.Assets == nil {
		return link, blocked("invalid individual shared link detail")
	}
	for _, a := range link.Assets {
		if a.ID == "" {
			return link, blocked("invalid shared link asset")
		}
	}
	return link, nil
}
func linkContains(link sharedLink, id string) bool {
	for _, a := range link.Assets {
		if a.ID == id {
			return true
		}
	}
	return false
}

// Discover from all individual link details, never from the truncated previews.
func (c *Client) originalSharedLinks(ctx context.Context, source string) ([]string, error) {
	var links []sharedLink
	if e := c.json(ctx, "GET", "/shared-links", nil, &links); e != nil {
		return nil, e
	}
	if links == nil {
		return nil, blocked("invalid shared link list")
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, preview := range links {
		if preview.ID == "" || seen[preview.ID] {
			return nil, blocked("invalid shared link list")
		}
		seen[preview.ID] = true
		switch preview.Type {
		case "ALBUM":
			continue
		case "INDIVIDUAL":
		default:
			return nil, blocked("unknown shared link type")
		}
		detail, e := c.sharedLinkDetail(ctx, preview.ID)
		if e != nil {
			return nil, e
		}
		if linkContains(preview, source) && !linkContains(detail, source) {
			return nil, fmt.Errorf("shared link %s source detail not ready", preview.ID)
		}
		if linkContains(detail, source) {
			ids = append(ids, preview.ID)
		}
	}
	return ids, nil
}

// The full detail query joins EXIF rows. Retry briefly and return a retryable
// error if either asset is not yet visible; never infer absence from previews.
func (c *Client) verifySharedLinks(ctx context.Context, ids []string, source, target string) error {
	for _, id := range ids {
		ready := false
		for attempt := 0; attempt < 3; attempt++ {
			detail, e := c.sharedLinkDetail(ctx, id)
			if e != nil {
				return e
			}
			if linkContains(detail, source) && linkContains(detail, target) {
				ready = true
				break
			}
		}
		if !ready {
			return fmt.Errorf("shared link %s asset details not ready or membership missing", id)
		}
	}
	return nil
}
func (c *Client) migrate(ctx context.Context, in Input, id string) error {
	a, e := c.original(ctx, in)
	if e != nil {
		return e
	}
	albums, e := c.albums(ctx, in.OriginalAssetID)
	if e != nil {
		return e
	}
	links, e := c.originalSharedLinks(ctx, in.OriginalAssetID)
	if e != nil {
		return e
	}
	if e = c.migrateCustomMetadata(ctx, in.OriginalAssetID, id); e != nil {
		return e
	}
	// stack=false prevents the non-idempotent stack merge operation. Sidecar copying
	// is intentionally disabled because asynchronous extraction can overwrite edits.
	copyBody := map[string]any{"sourceId": in.OriginalAssetID, "targetId": id, "albums": true, "favorite": true, "sharedLinks": true, "sidecar": false, "stack": false}
	if e = c.json(ctx, "PUT", "/assets/copy", copyBody, nil); e != nil {
		return e
	}
	if e = c.verifySharedLinks(ctx, links, in.OriginalAssetID, id); e != nil {
		return e
	}
	update := map[string]any{"ids": []string{id}, "isFavorite": a.IsFavorite, "visibility": a.Visibility}
	for _, k := range migratedExifFields {
		if v, ok := a.Exif[k]; ok && v != nil {
			update[k] = v
		}
	}
	if e = c.json(ctx, "PUT", "/assets", update, nil); e != nil {
		return e
	}
	for _, raw := range a.Tags {
		var tag struct {
			ID string `json:"id"`
		}
		if e = json.Unmarshal(raw, &tag); e != nil || tag.ID == "" {
			return blocked("invalid source tag")
		}
		var response []struct {
			ID      string `json:"id"`
			Success bool   `json:"success"`
		}
		if e = c.json(ctx, "PUT", "/tags/"+pathID(tag.ID)+"/assets", map[string]any{"ids": []string{id}}, &response); e != nil {
			return e
		}
	}
	for _, al := range albums {
		if al.Cover != nil && *al.Cover == in.OriginalAssetID {
			var ignored any
			if e = c.json(ctx, "PATCH", "/albums/"+pathID(al.ID), map[string]any{"albumThumbnailAssetId": id}, &ignored); e != nil {
				return e
			}
		}
	}
	return c.verifyRelations(ctx, in, id)
}
func (c *Client) verifyRelations(ctx context.Context, in Input, id string) error {
	if e := c.verifyCustomMetadata(ctx, in.OriginalAssetID, id); e != nil {
		return e
	}
	a, e := c.original(ctx, in)
	if e != nil {
		return e
	}
	b, e := c.getAsset(ctx, id)
	if e != nil {
		return e
	}
	tagIDs := func(tags []json.RawMessage) map[string]bool {
		m := map[string]bool{}
		for _, raw := range tags {
			var tag struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &tag) == nil && tag.ID != "" {
				m[tag.ID] = true
			}
		}
		return m
	}
	sourceTags, targetTags := tagIDs(a.Tags), tagIDs(b.Tags)
	if len(sourceTags) != len(a.Tags) {
		return blocked("invalid source tags")
	}
	for id := range sourceTags {
		if !targetTags[id] {
			return blocked("tag %s missing", id)
		}
	}
	if a.IsFavorite != b.IsFavorite || a.Visibility != b.Visibility {
		return blocked("favorite or visibility mismatch")
	}
	for _, k := range migratedExifFields {
		if v, ok := a.Exif[k]; ok && v != nil && !reflect.DeepEqual(v, b.Exif[k]) {
			if k == "timeZone" && equivalentCaptureTimeZone(a.Exif, b.Exif) {
				continue
			}
			return blocked("metadata mismatch: %s", k)
		}
	}
	source, e := c.albums(ctx, in.OriginalAssetID)
	if e != nil {
		return e
	}
	target, e := c.albums(ctx, id)
	if e != nil {
		return e
	}
	have := albumSet(target)
	for _, al := range source {
		if !have[al.ID] {
			return blocked("album %s missing", al.ID)
		}
		if al.Cover != nil && *al.Cover == in.OriginalAssetID {
			return blocked("album %s still uses original cover", al.ID)
		}
	}
	links, e := c.originalSharedLinks(ctx, in.OriginalAssetID)
	if e != nil {
		return e
	}
	return c.verifySharedLinks(ctx, links, in.OriginalAssetID, id)
}

// Publish uploads, verifies and migrates. Every callback must durably commit its
// stage and mapping. A failure retains the original; retries reconcile by hash
// and provenance. The caller must serialize attempts for the same owner/output.
func (c *Client) Publish(ctx context.Context, in Input, checkpoint Checkpoint) (Result, error) {
	var zero Result
	if checkpoint == nil {
		return zero, blocked("checkpoint callback required")
	}
	var me struct {
		ID string `json:"id"`
	}
	if e := c.json(ctx, "GET", "/users/me", nil, &me); e != nil {
		return zero, blocked("cannot verify API key owner: %v", e)
	}
	if me.ID != in.OwnerID {
		return zero, blocked("API key owner mismatch")
	}

	if e := c.validateInput(ctx, in); e != nil {
		return zero, e
	}
	a, e := c.original(ctx, in)
	if e != nil {
		return zero, e
	}
	id := in.OutputAssetID
	if id == "" {
		id, e = c.duplicate(ctx, in)
		if e != nil {
			return zero, e
		}
		if id == "" {
			id, e = c.upload(ctx, in, a)
			if e != nil {
				return zero, e
			}
		}
	}
	r := result(in, id)
	if e = checkpoint(ctx, StageUploaded, r); e != nil {
		return r, e
	}
	if e = c.output(ctx, in, id); e != nil {
		return r, e
	}
	if _, e = c.original(ctx, in); e != nil {
		return r, e
	}
	if e = checkpoint(ctx, StageVerified, r); e != nil {
		return r, e
	}
	if e = c.migrate(ctx, in, id); e != nil {
		return r, e
	}
	if e = checkpoint(ctx, StageMetadataMigrated, r); e != nil {
		return r, e
	}
	return r, nil
}

// Retire is opt-in and requires that the caller has committed the mapping in its
// ledger. Disabled is a safe keep-original mode. Immich does not offer atomic
// compare-and-trash, so callers must avoid concurrent user edits/deletions.
func (c *Client) Retire(ctx context.Context, in Input, r Result, enabled bool) error {
	if !enabled {
		return nil
	}
	if !reflect.DeepEqual(r, result(in, r.OutputAssetID)) || r.OutputAssetID == "" {
		return blocked("invalid mapping")
	}
	// A prior DELETE may have succeeded even if its response was lost. Validate
	// the durable mapping and the replacement before accepting that state. This
	// path does not require staged files or callbacks that need an active original.
	if e := c.output(ctx, in, r.OutputAssetID); e != nil {
		return e
	}
	original, e := c.getAsset(ctx, in.OriginalAssetID)
	if e != nil {
		return blocked("cannot inspect original: %v", e)
	}
	if original.ID != in.OriginalAssetID || original.OwnerID != in.OwnerID || !checksum(original, in.SourceSHA1) {
		return blocked("original identity, owner or checksum mismatch")
	}
	if original.IsTrashed {
		return nil
	}
	if e := c.validateInput(ctx, in); e != nil {
		return e
	}
	if e := c.verifyRelations(ctx, in, r.OutputAssetID); e != nil {
		return e
	}
	var features struct {
		Trash bool `json:"trash"`
	}
	if e := c.json(ctx, "GET", "/server/features", nil, &features); e != nil {
		return e
	}
	if !features.Trash {
		return blocked("Immich trash disabled")
	}
	if _, e := c.original(ctx, in); e != nil {
		return e
	}
	return c.json(ctx, "DELETE", "/assets", map[string]any{"ids": []string{in.OriginalAssetID}, "force": false}, nil)
}
