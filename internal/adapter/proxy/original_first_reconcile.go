package proxy

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

// ReconcileOriginalIntent resolves one durable, unacknowledged original upload.
// The caller must persist the returned ID with MarkOriginalUploaded.
func (s *Server) ReconcileOriginalIntent(ctx context.Context, job jobqueue.Job, apiKey string) (string, error) {
	if s.saver == nil || strings.TrimSpace(apiKey) == "" || job.Status != jobqueue.StatusPublishingOriginal || job.OriginalAssetID != "" || job.OwnerID == "" {
		return "", errors.New("invalid original ingestion intent")
	}
	select {
	case s.semaphore <- struct{}{}:
		defer func() { <-s.semaphore }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/assets", nil)
	if err != nil {
		return "", err
	}
	r.Header.Set("x-api-key", apiKey)
	owner, err := s.owner(r)
	if err != nil || owner != job.OwnerID {
		return "", errors.New("background API key does not identify the intent owner")
	}
	id, err := s.checkOriginalDuplicate(r, job.SourceSHA1)
	if err != nil || id != "" {
		return id, err
	}

	parts, err := s.reconcileParts(job)
	if err != nil {
		return "", err
	}
	body, err := os.CreateTemp(filepath.Join(s.saver.dir, "spool"), ".reconcile-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close(); _ = os.Remove(body.Name()) }()
	mw := multipart.NewWriter(body)
	if err := writeOriginalFirstParts(mw, parts); err != nil {
		return "", err
	}
	info, err := body.Stat()
	if err != nil {
		return "", err
	}
	if _, err = body.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	req, err := s.upstreamRequest(r, http.MethodPost, "/api/assets", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = info.Size()
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if len(data) > 1<<20 || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) {
		return "", fmt.Errorf("original reconciliation upload HTTP %d", resp.StatusCode)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.ID == "" {
		return "", errors.New("original reconciliation returned no asset ID")
	}
	return result.ID, nil
}

// resolvedManagedFile requires a regular file under an expected managed root,
// rejecting symlink escapes in either the file or its parent directories.
func resolvedManagedFile(root, name string) (string, error) {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	resolvedRoot, err = filepath.Abs(resolvedRoot)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", errors.New("file outside managed root")
	}
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("managed file is not regular")
	}
	return resolved, nil
}

func (s *Server) reconcileParts(job jobqueue.Job) ([]originalFirstPart, error) {
	paths := job.UploadFields[originalFirstMultipartField]
	if len(paths) != 1 || filepath.Base(paths[0]) != "manifest.json" {
		return nil, errors.New("missing multipart manifest")
	}
	root := filepath.Join(s.saver.dir, "original-first-parts")
	manifest, err := resolvedManagedFile(root, paths[0])
	if err != nil {
		return nil, fmt.Errorf("invalid multipart manifest: %w", err)
	}
	if !strings.HasPrefix(filepath.Base(filepath.Dir(manifest)), "job-") {
		return nil, errors.New("invalid multipart directory")
	}
	f, err := os.Open(manifest)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	_ = f.Close()
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("invalid multipart manifest size")
	}
	var extras []originalFirstPart
	if err := json.Unmarshal(data, &extras); err != nil {
		return nil, fmt.Errorf("invalid multipart manifest: %w", err)
	}
	for i := range extras {
		path, err := resolvedManagedFile(filepath.Dir(manifest), extras[i].Path)
		if err != nil || filepath.Dir(path) != filepath.Dir(manifest) || filepath.Base(path) != fmt.Sprintf("part-%d", i) {
			return nil, errors.New("invalid multipart part path")
		}
		_, params, err := mime.ParseMediaType(extras[i].Header.Get("Content-Disposition"))
		if err != nil || params["name"] == "" || params["name"] == s.filterFormKey {
			return nil, errors.New("invalid multipart part header")
		}
		extras[i].Path = path
	}
	source, err := resolvedManagedFile(filepath.Join(s.saver.dir, "queue", "spool"), job.SourceSpoolPath)
	if err != nil {
		return nil, fmt.Errorf("invalid source path: %w", err)
	}
	in, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	h1, h256 := sha1.New(), sha256.New()
	n, err := io.Copy(io.MultiWriter(h1, h256), in)
	_ = in.Close()
	if err != nil || n != job.Size || hex.EncodeToString(h1.Sum(nil)) != strings.ToLower(job.SourceSHA1) || hex.EncodeToString(h256.Sum(nil)) != strings.ToLower(job.SourceSHA256) {
		return nil, errors.New("source content does not match intent")
	}
	if job.Filename == "" || filepath.Base(job.Filename) != job.Filename {
		return nil, errors.New("invalid source filename")
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": s.filterFormKey, "filename": job.Filename}))
	contentType := mime.TypeByExtension(filepath.Ext(job.Filename))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	return append(extras, originalFirstPart{Header: header, Path: source}), nil
}
