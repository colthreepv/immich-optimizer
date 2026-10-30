package coordinator

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/textproto"
	"os"

	"github.com/colthreepv/immich-optimizer/internal/adapter/replacement"
	"github.com/colthreepv/immich-optimizer/internal/jobqueue"
)

// Sidecar bytes remain durable in the intent, but this release cannot prove
// their asynchronous extraction safe when retiring the original.
func auditQueuedParts(job jobqueue.Job) error {
	paths := job.UploadFields["_iuo_multipart_v1"]
	if len(paths) == 0 {
		return nil
	}
	if len(paths) != 1 {
		return fmt.Errorf("%w: invalid multipart manifest", replacement.ErrBlocked)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return err
	}
	defer f.Close()
	var parts []struct {
		Header textproto.MIMEHeader `json:"header"`
	}
	if err = json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&parts); err != nil {
		return fmt.Errorf("%w: invalid multipart manifest", replacement.ErrBlocked)
	}
	for _, part := range parts {
		_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if err != nil {
			return fmt.Errorf("%w: invalid multipart disposition", replacement.ErrBlocked)
		}
		if params["filename"] != "" {
			return fmt.Errorf("%w: sidecar file requires migration support", replacement.ErrBlocked)
		}
	}
	return nil
}
