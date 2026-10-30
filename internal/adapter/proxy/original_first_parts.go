package proxy

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
)

// originalFirstPart preserves order, repeated fields and the original MIME
// headers. File bytes are kept separately so neither sidecars nor metadata are
// lost when the original is published or a job is retried.
type originalFirstPart struct {
	Header textproto.MIMEHeader `json:"header"`
	Path   string               `json:"path"`
}

type originalFirstParts struct {
	Parts          []originalFirstPart `json:"parts"`
	SourcePath     string              `json:"source_path"`
	SourceFilename string              `json:"source_filename"`
	SourceSHA1     string              `json:"source_sha1"`
	SourceSHA256   string              `json:"source_sha256"`
	SourceSize     int64               `json:"source_size"`
}

// The queue stores this manifest path with the job. The manifest retains all
// non-source multipart headers and paths to synced bytes, including sidecars.
const originalFirstMultipartField = "_iuo_multipart_v1"

func originalFirstUploadFields(stateDir string, parts originalFirstParts) (map[string][]string, error) {
	root := filepath.Join(stateDir, "original-first-parts")
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(root, "job-*")
	if err != nil {
		return nil, err
	}
	var extras []originalFirstPart
	for _, part := range parts.Parts {
		if part.Path == parts.SourcePath {
			continue
		}
		path := filepath.Join(dir, fmt.Sprintf("part-%d", len(extras)))
		if err := durableCopy(part.Path, path); err != nil {
			return nil, err
		}
		extras = append(extras, originalFirstPart{Header: part.Header, Path: path})
	}
	data, err := json.Marshal(extras)
	if err != nil {
		return nil, err
	}
	manifest := filepath.Join(dir, "manifest.json")
	f, err := os.OpenFile(manifest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	for _, path := range []string{dir, root, stateDir} {
		if d, err := os.Open(path); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return map[string][]string{originalFirstMultipartField: {manifest}}, nil
}

// captureOriginalFirstParts streams each part to its own synced file in dir.
// The caller owns dir and removes it on failure or when durable job ownership
// has been transferred. Paths are generated locally, never from client names.
func captureOriginalFirstParts(r *http.Request, dir, formKey string) (originalFirstParts, error) {
	var result originalFirstParts
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return result, fmt.Errorf("invalid multipart content type")
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("read multipart: %w", err)
		}
		file, err := os.CreateTemp(dir, "part-*")
		if err != nil {
			part.Close()
			return result, err
		}
		isSource := part.FormName() == formKey && part.FileName() != ""
		if isSource && result.SourcePath != "" {
			file.Close()
			part.Close()
			return result, fmt.Errorf("exactly one source file is required")
		}
		hash := sha1.New()
		strongHash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(file, hash, strongHash), part)
		part.Close()
		if copyErr == nil {
			copyErr = file.Sync()
		}
		if closeErr := file.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return result, copyErr
		}
		result.Parts = append(result.Parts, originalFirstPart{Header: textproto.MIMEHeader(http.Header(part.Header).Clone()), Path: file.Name()})
		if isSource {
			result.SourcePath = file.Name()
			result.SourceFilename = filepath.Base(part.FileName())
			result.SourceSHA1 = hex.EncodeToString(hash.Sum(nil))
			result.SourceSHA256 = hex.EncodeToString(strongHash.Sum(nil))
			result.SourceSize = n
		}
	}
	if result.SourcePath == "" {
		return result, fmt.Errorf("exactly one source file is required")
	}
	return result, nil
}

func writeOriginalFirstParts(w *multipart.Writer, parts []originalFirstPart) error {
	for _, part := range parts {
		in, err := os.Open(part.Path)
		if err != nil {
			return err
		}
		out, err := w.CreatePart(part.Header)
		if err == nil {
			_, err = io.Copy(out, in)
		}
		if closeErr := in.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return w.Close()
}
