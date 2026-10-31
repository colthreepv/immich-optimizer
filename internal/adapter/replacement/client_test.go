package replacement

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type fixture struct {
	mu                                                                                        sync.Mutex
	sourceMeta, targetMeta                                                                    map[string]map[string]any
	sourceExif, outputExif                                                                    map[string]any
	outputOwner                                                                               string
	linkAssets                                                                                []string
	linkPreview                                                                               string
	linkCopyMissing                                                                           bool
	linkDetailError                                                                           bool
	linkHideOutputReads                                                                       int
	outputTrashed, outputOffline, outputFavorite, omitFavorite                                bool
	republished                                                                               []bool
	dropMetaWrite, offline, missingOriginal, sourcePending, outputPending                     bool
	faces, people                                                                             []any
	t                                                                                         *testing.T
	input                                                                                     Input
	uploaded, copied, updated, trashed, badDownload, lostACK, lostDeleteACK, edited, activity bool
	originalOwner, originalSHA1                                                               string
	stages                                                                                    []Stage
	requests                                                                                  []string
	meta                                                                                      map[string]any
}

func newFixture(t *testing.T) (*fixture, *Client) {
	t.Helper()
	b := []byte("optimized avif data")
	p := filepath.Join(t.TempDir(), "output.avif")
	if e := os.WriteFile(p, b, 0600); e != nil {
		t.Fatal(e)
	}
	x := sha1.Sum(b)
	y := sha256.Sum256(b)
	z := sha1.Sum([]byte("source"))
	f := &fixture{t: t, sourceMeta: map[string]map[string]any{"user-note": {"nested": map[string]any{"label": "hello"}}, provenanceKey: {"old": "original"}}, targetMeta: map[string]map[string]any{}, faces: []any{}, input: Input{OwnerID: "owner", OriginalAssetID: "original", InputName: "a.jpg", OutputName: "output.avif", SourceSize: 6, OutputSize: int64(len(b)), Preset: "small", OutputPath: p, OutputSHA1: hex.EncodeToString(x[:]), OutputSHA256: hex.EncodeToString(y[:]), SourceSHA1: hex.EncodeToString(z[:]), VerifyAVIF: func(context.Context, string, string) error { return nil }, AuditRelations: func(context.Context, string, string) error { return nil }}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, NewClient(srv.URL, "key", srv.Client())
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) {
		if e := json.NewEncoder(w).Encode(v); e != nil {
			f.t.Error(e)
		}
	}
	asset := func(id, sha string) map[string]any {
		raw, _ := hex.DecodeString(sha)
		size := f.input.OutputSize
		if id == "original" {
			size = f.input.SourceSize
		}
		exif := map[string]any{"description": "hello", "fileSizeInByte": size}
		extra := f.outputExif
		if id == "original" {
			extra = f.sourceExif
		}
		for k, v := range extra {
			exif[k] = v
		}
		return map[string]any{"id": id, "ownerId": "owner", "checksum": base64.StdEncoding.EncodeToString(raw), "type": "IMAGE", "visibility": "timeline", "width": 640, "height": 480, "fileCreatedAt": "2024-01-01T00:00:00Z", "fileModifiedAt": "2024-01-01T00:00:00Z", "exifInfo": exif}
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/users/me":
		write(map[string]any{"id": "owner"})
	case "GET /api/assets/original":
		if f.missingOriginal {
			http.Error(w, "missing", http.StatusNotFound)
			return
		}
		sha := f.input.SourceSHA1
		if f.originalSHA1 != "" {
			sha = f.originalSHA1
		}
		a := asset("original", sha)
		if f.originalOwner != "" {
			a["ownerId"] = f.originalOwner
		}
		a["isTrashed"] = f.trashed
		a["isOffline"] = f.offline
		a["isEdited"] = f.edited
		if f.sourcePending {
			a["exifInfo"] = map[string]any{}
			a["width"] = nil
		}
		if f.people != nil {
			a["people"] = f.people
		}
		write(a)
	case "GET /api/assets/output":
		a := asset("output", f.input.OutputSHA1)
		if f.outputOwner != "" {
			a["ownerId"] = f.outputOwner
		}
		a["isTrashed"] = f.outputTrashed
		a["isOffline"] = f.outputOffline
		if !f.omitFavorite {
			a["isFavorite"] = f.outputFavorite
		}
		if !f.updated {
			a["exifInfo"] = map[string]any{"fileSizeInByte": f.input.OutputSize}
		}
		if f.outputPending {
			a["exifInfo"] = map[string]any{}
			a["width"] = nil
		}
		write(a)
	case "PUT /api/assets/output":
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			f.t.Error(e)
		}
		favorite, ok := body["isFavorite"].(bool)
		if !ok || len(body) != 1 {
			f.t.Error("invalid republish body")
		}
		f.republished = append(f.republished, favorite)
		w.WriteHeader(200)
	case "GET /api/assets/original/metadata":
		items := []metadata{}
		for k, v := range f.sourceMeta {
			items = append(items, metadata{Key: k, Value: v})
		}
		write(items)
	case "GET /api/assets/output/metadata":
		items := []metadata{{Key: provenanceKey, Value: f.meta}}
		for k, v := range f.targetMeta {
			items = append(items, metadata{Key: k, Value: v})
		}
		write(items)
	case "PUT /api/assets/output/metadata":
		var b struct {
			Items []metadata `json:"items"`
		}
		if e := json.NewDecoder(r.Body).Decode(&b); e != nil {
			f.t.Error(e)
		}
		if !f.dropMetaWrite {
			for _, v := range b.Items {
				f.targetMeta[v.Key] = v.Value
			}
		}
		write(b.Items)
	case "GET /api/assets/output/metadata/storage-saver-source":
		write(map[string]any{"value": f.meta})
	case "GET /api/assets/original/original":
		fmt.Fprint(w, "source")
	case "GET /api/assets/output/original":
		if f.badDownload {
			fmt.Fprint(w, "wrong")
		} else {
			fmt.Fprint(w, "optimized avif data")
		}
	case "POST /api/assets/bulk-upload-check":
		if f.uploaded {
			write(map[string]any{"results": []any{map[string]any{"id": "output", "action": "reject", "reason": "duplicate", "assetId": "output"}}})
		} else {
			write(map[string]any{"results": []any{map[string]any{"id": "output", "action": "accept"}}})
		}
	case "POST /api/assets":
		if e := r.ParseMultipartForm(1024 * 1024); e != nil {
			f.t.Error(e)
		}
		var m []struct {
			Value map[string]any `json:"value"`
		}
		if e := json.Unmarshal([]byte(r.FormValue("metadata")), &m); e != nil || len(m) != 1 {
			f.t.Errorf("metadata: %v", e)
		} else {
			f.meta = m[0].Value
		}
		f.uploaded = true
		if f.lostACK {
			f.lostACK = false
			h := w.(http.Hijacker)
			c, _, _ := h.Hijack()
			c.Close()
			return
		}
		write(map[string]any{"id": "output", "status": "created"})
	case "GET /api/albums":
		if r.URL.Query().Get("assetId") == "original" || f.copied {
			write([]any{map[string]any{"id": "album", "albumThumbnailAssetId": "other"}})
		} else {
			write([]any{})
		}
	case "PUT /api/assets/copy":
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["stack"] != false {
			f.t.Error("stack must be false")
		}
		f.copied = true
		if !f.linkCopyMissing && len(f.linkAssets) > 0 {
			found := false
			for _, v := range f.linkAssets {
				if v == "output" {
					found = true
				}
			}
			if !found {
				f.linkAssets = append(f.linkAssets, "output")
			}
		}
		w.WriteHeader(204)
	case "PUT /api/assets":
		f.updated = true
		w.WriteHeader(204)
	case "GET /api/faces":
		write(f.faces)
	case "GET /api/activities":
		if f.activity {
			write([]any{map[string]any{"id": "comment"}})
		} else {
			write([]any{})
		}
	case "GET /api/shared-links":
		if f.linkAssets == nil {
			write([]any{})
			break
		}
		preview := f.linkAssets[0]
		if f.linkPreview != "" {
			preview = f.linkPreview
		}
		write([]any{map[string]any{"id": "link", "type": "INDIVIDUAL", "assets": []any{map[string]any{"id": preview}}}})
	case "GET /api/shared-links/link":
		if f.linkDetailError {
			http.Error(w, "not found", 404)
			break
		}
		assets := []any{}
		for _, v := range f.linkAssets {
			if v == "output" && f.linkHideOutputReads > 0 {
				continue
			}
			assets = append(assets, map[string]any{"id": v, "ownerId": "owner", "checksum": "fixture"})
		}
		if f.copied && f.linkHideOutputReads > 0 {
			f.linkHideOutputReads--
		}
		write(map[string]any{"id": "link", "type": "INDIVIDUAL", "assets": assets})
	case "GET /api/server/features":
		write(map[string]any{"trash": true})
	case "DELETE /api/assets":
		f.trashed = true
		if f.lostDeleteACK {
			f.lostDeleteACK = false
			h := w.(http.Hijacker)
			conn, _, _ := h.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(204)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		http.Error(w, "unexpected", 500)
	}
}
func (f *fixture) checkpoint(_ context.Context, s Stage, _ Result) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stages = append(f.stages, s)
	return nil
}
func (f *fixture) locked(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }
func (f *fixture) wasTrashed() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.trashed }
func (f *fixture) requestList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}
func (f *fixture) stageList() []Stage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Stage(nil), f.stages...)
}
func TestLostACKReconcilesAndTrashIsLast(t *testing.T) {
	f, c := newFixture(t)
	f.locked(func() { f.lostACK = true })
	ctx := context.Background()
	if _, e := c.Publish(ctx, f.input, f.checkpoint); e == nil {
		t.Fatal("expected lost ACK")
	}
	r, e := c.Publish(ctx, f.input, f.checkpoint)
	if e != nil {
		t.Fatal(e)
	}
	if r.OutputAssetID != "output" || !reflect.DeepEqual(f.stageList(), []Stage{StageUploaded, StageVerified, StageMetadataMigrated}) {
		t.Fatalf("result/stages: %+v %v", r, f.stageList())
	}
	if f.wasTrashed() {
		t.Fatal("publish trashed original")
	}
	if e = c.Retire(ctx, f.input, r, false); e != nil || f.wasTrashed() {
		t.Fatal("keep-original mode", e)
	}
	if e = c.Retire(ctx, f.input, r, true); e != nil {
		t.Fatal(e)
	}
	req := f.requestList()
	if !f.wasTrashed() || req[len(req)-1] != "DELETE /api/assets" {
		t.Fatal("trash not last")
	}
	uploads := 0
	for _, x := range req {
		if x == "POST /api/assets" {
			uploads++
		}
	}
	if uploads != 1 {
		t.Fatalf("uploads %d", uploads)
	}
}
func TestBadOutputNeverMigratesOrTrashes(t *testing.T) {
	f, c := newFixture(t)
	f.locked(func() { f.badDownload = true })
	_, e := c.Publish(context.Background(), f.input, f.checkpoint)
	f.locked(func() {
		if e == nil || f.copied || f.trashed {
			t.Errorf("bad output accepted: %v", e)
		}
	})
}
func TestCheckpointFailureStopsProgress(t *testing.T) {
	f, c := newFixture(t)
	_, e := c.Publish(context.Background(), f.input, func(context.Context, Stage, Result) error { return io.ErrClosedPipe })
	f.locked(func() {
		if e == nil || f.copied || f.trashed {
			t.Errorf("checkpoint failure ignored: %v", e)
		}
	})
}
func TestUnsupportedOriginalBlockedBeforeUpload(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fixture)
	}{{"edited", func(f *fixture) { f.edited = true }}, {"comment", func(f *fixture) { f.activity = true }}} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			f.locked(func() { tc.set(f) })
			_, e := c.Publish(context.Background(), f.input, f.checkpoint)
			f.locked(func() {
				if !errors.Is(e, ErrBlocked) || f.uploaded || f.trashed {
					t.Errorf("unsafe result: %v", e)
				}
			})
		})
	}
}
func TestRetireLostACKReconcilesTrashedOriginal(t *testing.T) {
	f, c := newFixture(t)
	ctx := context.Background()
	r, e := c.Publish(ctx, f.input, f.checkpoint)
	if e != nil {
		t.Fatal(e)
	}
	if r.SourceSize == nil || *r.SourceSize != f.input.SourceSize || r.OutputSize == nil || *r.OutputSize != f.input.OutputSize {
		t.Fatalf("sizes: %+v", r)
	}
	f.locked(func() { f.lostDeleteACK = true })
	if e = c.Retire(ctx, f.input, r, true); e == nil || !f.wasTrashed() {
		t.Fatalf("expected lost response: %v", e)
	}
	if e = os.Remove(f.input.OutputPath); e != nil {
		t.Fatal(e)
	}
	in := f.input
	in.VerifyAVIF = func(context.Context, string, string) error { return errors.New("should not run") }
	if e = c.Retire(ctx, in, r, true); e != nil {
		t.Fatal(e)
	}
	deletes := 0
	for _, request := range f.requestList() {
		if request == "DELETE /api/assets" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("deletes %d", deletes)
	}
}
func TestRetireTrashedOriginalRequiresMatchingIdentityAndVerifiedOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*fixture, *Result)
	}{{"owner", func(f *fixture, _ *Result) { f.originalOwner = "other" }}, {"checksum", func(f *fixture, _ *Result) { f.originalSHA1 = strings.Repeat("0", 40) }}, {"output", func(f *fixture, _ *Result) { f.badDownload = true }}, {"provenance", func(f *fixture, _ *Result) { f.meta["sourceSHA1"] = strings.Repeat("0", 40) }}, {"mapping size", func(f *fixture, r *Result) { v := int64(999); r.SourceSize = &v }}} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			ctx := context.Background()
			r, e := c.Publish(ctx, f.input, f.checkpoint)
			if e != nil {
				t.Fatal(e)
			}
			f.locked(func() { f.trashed = true; tc.damage(f, &r) })
			if e = c.Retire(ctx, f.input, r, true); e == nil {
				t.Fatal("accepted mismatch")
			}
			for _, req := range f.requestList() {
				if req == "DELETE /api/assets" {
					t.Fatal("unexpected delete")
				}
			}
		})
	}
}
func TestCustomMetadataCopiedAndVerified(t *testing.T) {
	f, c := newFixture(t)
	r, e := c.Publish(context.Background(), f.input, f.checkpoint)
	if e != nil {
		t.Fatal(e)
	}
	f.locked(func() {
		if !reflect.DeepEqual(f.sourceMeta["user-note"], f.targetMeta["user-note"]) {
			t.Error("metadata missing")
		}
		if _, ok := f.targetMeta[provenanceKey]; ok {
			t.Error("overwrote provenance")
		}
	})
	f.locked(func() { f.targetMeta["user-note"] = map[string]any{"changed": true} })
	if e = c.Retire(context.Background(), f.input, r, true); !errors.Is(e, ErrBlocked) {
		t.Fatalf("expected mismatch block: %v", e)
	}
	if f.wasTrashed() {
		t.Fatal("trashed with changed metadata")
	}
}
func TestCustomMetadataMissingOrConflictingBlocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*fixture)
	}{{"missing readback", func(f *fixture) { f.dropMetaWrite = true }}, {"target conflict", func(f *fixture) { f.targetMeta["user-note"] = map[string]any{"other": true} }}} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			f.locked(func() { tc.set(f) })
			_, e := c.Publish(context.Background(), f.input, f.checkpoint)
			if !errors.Is(e, ErrBlocked) {
				t.Fatalf("expected block: %v", e)
			}
			if f.wasTrashed() {
				t.Fatal("original trashed")
			}
		})
	}
}

func TestFaceSafety(t *testing.T) {
	tests := []struct {
		name    string
		faces   []any
		people  []any
		allowed bool
	}{
		{"unassigned machine-learning", []any{map[string]any{"sourceType": "machine-learning", "person": nil}}, nil, true},
		{"unassigned machine-learning null personId", []any{map[string]any{"sourceType": "machine-learning", "person": nil, "personId": nil}}, nil, true},
		{"manual", []any{map[string]any{"sourceType": "manual", "person": nil}}, nil, false},
		{"named person", []any{map[string]any{"sourceType": "machine-learning", "person": map[string]any{"id": "person", "name": "Alice"}}}, nil, false},
		{"personId assigned", []any{map[string]any{"sourceType": "machine-learning", "person": nil, "personId": "person"}}, nil, false},
		{"asset people assigned", []any{map[string]any{"sourceType": "machine-learning", "person": nil}}, []any{map[string]any{"id": "person", "name": "Alice"}}, false},
		{"exif", []any{map[string]any{"sourceType": "exif", "person": nil}}, nil, false},
		{"unknown source", []any{map[string]any{"sourceType": "other", "person": nil}}, nil, false},
		{"missing source", []any{map[string]any{"person": nil}}, nil, false},
		{"missing person", []any{map[string]any{"sourceType": "machine-learning"}}, nil, false},
		{"invalid face", []any{nil}, nil, false},
		{"invalid response", nil, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			f.locked(func() { f.faces = tc.faces; f.people = tc.people })
			_, e := c.Publish(context.Background(), f.input, f.checkpoint)
			if tc.allowed {
				if e != nil {
					t.Fatal(e)
				}
			} else if !errors.Is(e, ErrBlocked) {
				t.Fatalf("expected hold: %v", e)
			}
			if !tc.allowed {
				f.locked(func() {
					if f.uploaded || f.trashed {
						t.Error("unsafe write")
					}
				})
			}
		})
	}
}
func TestFaceAddedAfterPublishBlocksRetirement(t *testing.T) {
	f, c := newFixture(t)
	ctx := context.Background()
	r, e := c.Publish(ctx, f.input, f.checkpoint)
	if e != nil {
		t.Fatal(e)
	}
	f.locked(func() { f.faces = []any{map[string]any{"sourceType": "manual", "person": nil}} })
	if e = c.Retire(ctx, f.input, r, true); !errors.Is(e, ErrBlocked) {
		t.Fatalf("expected hold: %v", e)
	}
	if f.wasTrashed() {
		t.Fatal("original trashed")
	}
}

func TestCanReleaseOriginal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		set    func(*fixture)
		wantOK bool
	}{
		{"active with unsupported relationships", func(f *fixture) {
			f.edited = true
			f.people = []any{map[string]any{"id": "person"}}
			f.faces = []any{map[string]any{"sourceType": "manual", "person": nil}}
		}, true},
		{"trashed", func(f *fixture) { f.trashed = true }, false},
		{"offline", func(f *fixture) { f.offline = true }, false},
		{"wrong owner", func(f *fixture) { f.originalOwner = "other" }, false},
		{"wrong checksum", func(f *fixture) { f.originalSHA1 = strings.Repeat("0", 40) }, false},
		{"missing", func(f *fixture) { f.missingOriginal = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			f.locked(func() { tc.set(f) })
			e := c.CanReleaseOriginal(context.Background(), f.input.OwnerID, f.input.OriginalAssetID, f.input.SourceSHA1)
			if (e == nil) != tc.wantOK {
				t.Fatalf("unexpected result: %v", e)
			}
			if req := f.requestList(); len(req) != 1 || req[0] != "GET /api/assets/original" {
				t.Fatalf("unexpected requests: %v", req)
			}
		})
	}
	f, c := newFixture(t)
	if e := c.CanReleaseOriginal(context.Background(), f.input.OwnerID, f.input.OriginalAssetID, "bad"); !errors.Is(e, ErrBlocked) {
		t.Fatalf("invalid checksum: %v", e)
	}
	if len(f.requestList()) != 0 {
		t.Fatal("invalid input triggered request")
	}
}
func TestCanReleaseOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(*fixture, *Input)
		wantOK bool
	}{
		{"verified output without staged file", func(f *fixture, in *Input) { in.OutputPath = ""; in.VerifyAVIF = nil; in.AuditRelations = nil }, true},
		{"bad downloaded bytes", func(f *fixture, in *Input) { f.badDownload = true }, false},
		{"provenance mismatch", func(f *fixture, in *Input) { f.meta["sourceSHA1"] = strings.Repeat("0", 40) }, false},
		{"output size mismatch", func(f *fixture, in *Input) { in.OutputSize++ }, false},
		{"missing output ID", func(f *fixture, in *Input) { in.OutputAssetID = "" }, false},
		{"original as output", func(f *fixture, in *Input) { in.OutputAssetID = in.OriginalAssetID }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			r, e := c.Publish(context.Background(), f.input, f.checkpoint)
			if e != nil {
				t.Fatal(e)
			}
			in := f.input
			in.OutputAssetID = r.OutputAssetID
			f.locked(func() { tc.damage(f, &in) })
			e = c.CanReleaseOutput(context.Background(), in)
			if (e == nil) != tc.wantOK {
				t.Fatalf("unexpected result: %v", e)
			}
		})
	}
}

func TestMetadataExtractionPendingRetriesWithoutDuplicate(t *testing.T) {
	f, c := newFixture(t)
	ctx := context.Background()
	f.locked(func() { f.outputPending = true })
	r, e := c.Publish(ctx, f.input, f.checkpoint)
	if e == nil || errors.Is(e, ErrBlocked) {
		t.Fatalf("expected retryable pending error: %v", e)
	}
	if r.OutputAssetID != "output" || !reflect.DeepEqual(f.stageList(), []Stage{StageUploaded}) {
		t.Fatalf("output ID/checkpoint: %+v %v", r, f.stageList())
	}
	f.locked(func() {
		if f.copied || f.trashed {
			t.Error("migrated or trashed before extraction")
		}
		f.outputPending = false
	})
	in := f.input
	in.OutputAssetID = r.OutputAssetID
	if _, e = c.Publish(ctx, in, f.checkpoint); e != nil {
		t.Fatal(e)
	}
	count := 0
	for _, req := range f.requestList() {
		if req == "POST /api/assets" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("uploaded %d times", count)
	}
}
func TestOriginalMetadataExtractionPendingRetryable(t *testing.T) {
	f, c := newFixture(t)
	f.locked(func() { f.sourcePending = true })
	_, e := c.Publish(context.Background(), f.input, f.checkpoint)
	if e == nil || errors.Is(e, ErrBlocked) {
		t.Fatalf("expected retryable pending error: %v", e)
	}
	f.locked(func() {
		if f.uploaded || f.trashed {
			t.Error("original changed while metadata pending")
		}
	})
}
func TestMetadataReadinessRejectsMissingSizeOrDimensions(t *testing.T) {
	size := int64(100)
	good := int64(1)
	zero := int64(0)
	cases := []asset{{ID: "missing"}, {ID: "size", Exif: map[string]any{"fileSizeInByte": float64(99)}, Width: &good, Height: &good}, {ID: "width", Exif: map[string]any{"fileSizeInByte": float64(100)}, Width: &zero, Height: &good}, {ID: "height", Exif: map[string]any{"fileSizeInByte": float64(100)}, Width: &good}}
	for _, a := range cases {
		if e := metadataReady(a, size); e == nil || errors.Is(e, ErrBlocked) {
			t.Errorf("%s: expected retryable error: %v", a.ID, e)
		}
	}
}

func TestRepublishPreservesFavoriteAndChecksOwner(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setup        func(*fixture)
		wantError    bool
		wantFavorite bool
	}{
		{"favorite", func(f *fixture) { f.outputFavorite = true }, false, true},
		{"not favorite", func(f *fixture) { f.outputFavorite = false }, false, false},
		{"wrong owner", func(f *fixture) { f.outputOwner = "other" }, true, false},
		{"trashed", func(f *fixture) { f.outputTrashed = true }, true, false},
		{"offline", func(f *fixture) { f.outputOffline = true }, true, false},
		{"missing favorite", func(f *fixture) { f.omitFavorite = true }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			f.locked(func() { tc.setup(f) })
			e := c.Republish(context.Background(), f.input.OwnerID, "output")
			if (e != nil) != tc.wantError {
				t.Fatalf("error %v", e)
			}
			if tc.wantError && !errors.Is(e, ErrBlocked) {
				t.Fatalf("expected blocked: %v", e)
			}
			f.locked(func() {
				if tc.wantError && len(f.republished) != 0 {
					t.Error("unexpected PUT")
				}
				if !tc.wantError && (len(f.republished) != 1 || f.republished[0] != tc.wantFavorite) {
					t.Errorf("favorite writes: %v", f.republished)
				}
			})
		})
	}
}

func TestSharedLinkListPreviewUsesFullDetails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		assets  []string
		preview string
	}{
		{"preview original", []string{"original"}, "original"},
		{"preview output after prior copy", []string{"output", "original"}, "output"},
		{"preview unrelated", []string{"unrelated", "original"}, "unrelated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFixture(t)
			f.locked(func() { f.linkAssets = tc.assets; f.linkPreview = tc.preview })
			ctx := context.Background()
			r, e := c.Publish(ctx, f.input, f.checkpoint)
			if e != nil {
				t.Fatal(e)
			}
			if e = c.Retire(ctx, f.input, r, true); e != nil {
				t.Fatal(e)
			}
			if !f.wasTrashed() {
				t.Fatal("verified original not trashed")
			}
			calls := 0
			for _, req := range f.requestList() {
				if req == "GET /api/shared-links/link" {
					calls++
				}
			}
			if calls < 3 {
				t.Fatalf("expected pre/post/retirement details; got %d", calls)
			}
		})
	}
}
func TestSharedLinkMissingOutputRetriesAndKeepsOriginal(t *testing.T) {
	f, c := newFixture(t)
	f.locked(func() { f.linkAssets = []string{"original"}; f.linkCopyMissing = true })
	ctx := context.Background()
	r, e := c.Publish(ctx, f.input, f.checkpoint)
	if e == nil || errors.Is(e, ErrBlocked) || f.wasTrashed() {
		t.Fatalf("expected retryable missing detail; got %v", e)
	}
	if r.OutputAssetID != "output" || !reflect.DeepEqual(f.stageList(), []Stage{StageUploaded, StageVerified}) {
		t.Fatalf("unexpected checkpoint: %+v %v", r, f.stageList())
	}
	f.locked(func() { f.linkCopyMissing = false })
	in := f.input
	in.OutputAssetID = r.OutputAssetID
	if _, e = c.Publish(ctx, in, f.checkpoint); e != nil {
		t.Fatal(e)
	}
}
func TestSharedLinkDetailVisibilityRetriesBoundedly(t *testing.T) {
	f, c := newFixture(t)
	f.locked(func() { f.linkAssets = []string{"original"}; f.linkHideOutputReads = 2 })
	if _, e := c.Publish(context.Background(), f.input, f.checkpoint); e != nil {
		t.Fatal(e)
	}
}
func TestSharedLinkDetailReadFailureKeepsOriginal(t *testing.T) {
	f, c := newFixture(t)
	f.locked(func() { f.linkAssets = []string{"original"}; f.linkDetailError = true })
	_, e := c.Publish(context.Background(), f.input, f.checkpoint)
	if e == nil || f.wasTrashed() {
		t.Fatalf("expected detail error: %v", e)
	}
}
func TestSharedLinkNative335PayloadPreviewIsNotMembership(t *testing.T) {
	const source = "c4628b0e-6a3e-4c05-b618-be92114281cb"
	const output = "2a79c405-0572-4159-aa29-648852e17ce9"
	const link = "d3c591c3-df11-44ff-8a9a-76324cfbe7a8"
	for _, preview := range []string{source, output} {
		t.Run(preview, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/shared-links":
					fmt.Fprintf(w, `[{"id":%q,"type":"INDIVIDUAL","assets":[{"id":%q}]}]`, link, preview)
				case "/api/shared-links/" + link:
					fmt.Fprintf(w, `{"id":%q,"type":"INDIVIDUAL","assets":[{"id":%q,"ownerId":"owner","checksum":"base64"},{"id":%q,"ownerId":"owner","checksum":"base64"}]}`, link, source, output)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := NewClient(server.URL, "key", server.Client())
			ids, e := c.originalSharedLinks(context.Background(), source)
			if e != nil || !reflect.DeepEqual(ids, []string{link}) {
				t.Fatalf("discovery %v %v", ids, e)
			}
			if e = c.verifySharedLinks(context.Background(), ids, source, output); e != nil {
				t.Fatal(e)
			}
		})
	}
}
