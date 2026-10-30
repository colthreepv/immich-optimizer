package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ValidateAVIF independently checks a worker result before publication. The
// decoder's container transforms, not its embedded EXIF orientation, define
// how an AVIF is displayed. Any unavailable tool or unreadable data fails closed.
func ValidateAVIF(ctx context.Context, sourcePath, resultPath string) error {
	return validateAVIF(ctx, sourcePath, resultPath, runTool)
}

type toolRunner func(context.Context, string, ...string) ([]byte, error)

func validateAVIF(ctx context.Context, sourcePath, resultPath string, run toolRunner) error {
	if sourcePath == "" || resultPath == "" || sourcePath == resultPath {
		return errors.New("distinct source and result paths required")
	}
	source, err := readMetadata(ctx, sourcePath, run)
	if err != nil {
		return fmt.Errorf("source metadata: %w", err)
	}
	result, err := readMetadata(ctx, resultPath, run)
	if err != nil {
		return fmt.Errorf("result metadata: %w", err)
	}
	if source["MIMEType"] != "image/jpeg" {
		return errors.New("source is not JPEG")
	}
	if result["MIMEType"] != "image/avif" {
		return errors.New("result is not AVIF")
	}
	width, err := positiveInt(source["ImageWidth"])
	if err != nil {
		return fmt.Errorf("source width: %w", err)
	}
	height, err := positiveInt(source["ImageHeight"])
	if err != nil {
		return fmt.Errorf("source height: %w", err)
	}
	orientation := 1
	if source["Orientation"] != "" {
		orientation, err = strconv.Atoi(source["Orientation"])
		if err != nil || orientation < 1 || orientation > 8 {
			return errors.New("invalid source orientation")
		}
	}
	info, err := run(ctx, "avifdec", "-i", resultPath)
	if err != nil {
		return fmt.Errorf("read AVIF: %w", err)
	}
	resolution := regexp.MustCompile(`(?m)^\s*\* Resolution\s*:\s*(\d+)x(\d+)\s*$`).FindStringSubmatch(string(info))
	if len(resolution) != 3 {
		return errors.New("missing AVIF dimensions")
	}
	rw, _ := strconv.Atoi(resolution[1])
	rh, _ := strconv.Atoi(resolution[2])
	if rw != width || rh != height {
		return fmt.Errorf("dimensions changed: %dx%d to %dx%d", width, height, rw, rh)
	}
	// Require the same transforms as the existing pinned converter. Rotations are
	// counter-clockwise quarter turns; mirrors use the AVIF axis convention.
	rotation := [9]int{0, 0, 0, 2, 0, 1, 3, 3, 1}[orientation]
	mirror := [9]int{-1, -1, 1, -1, 0, 0, -1, 0, -1}[orientation]
	if err = checkTransform(string(info), `irot \(Rotation\)`, rotation, false); err != nil {
		return err
	}
	if err = checkTransform(string(info), `imir \(Mirror\)`, mirror, true); err != nil {
		return err
	}
	for _, pattern := range []string{`(?m)^\s*\* Format\s*:\s*YUV420\s*$`, `(?m)^\s*\* Color Primaries\s*:\s*\S+`, `(?m)^\s*\* Transfer Char\.\s*:\s*\S+`, `(?m)^\s*\* Matrix Coeffs\.?\s*:\s*\S+`} {
		if !regexp.MustCompile(pattern).Match(info) {
			return errors.New("missing or unsupported AVIF color signal")
		}
	}
	for _, tag := range []string{"DateTimeOriginal", "CreateDate", "OffsetTimeOriginal", "GPSLatitude", "GPSLongitude"} {
		if source[tag] != "" && source[tag] != result[tag] {
			return fmt.Errorf("metadata %s missing or changed", tag)
		}
	}
	before, err := run(ctx, "exiftool", "-q", "-q", "-b", "-ICC_Profile", sourcePath)
	if err != nil {
		return fmt.Errorf("source ICC: %w", err)
	}
	if len(before) > 0 {
		after, err := run(ctx, "exiftool", "-q", "-q", "-b", "-ICC_Profile", resultPath)
		if err != nil {
			return fmt.Errorf("result ICC: %w", err)
		}
		if !bytes.Equal(before, after) {
			return errors.New("ICC profile missing or changed")
		}
	}
	return nil
}
func positiveInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, errors.New("missing or invalid dimension")
	}
	return n, nil
}
func checkTransform(info, tag string, want int, mirror bool) error {
	re := regexp.MustCompile(`(?m)^\s*\* ` + tag + `\s*:\s*(\d+)(?:\s.*)?$`)
	m := re.FindStringSubmatch(info)
	if len(m) == 0 {
		if want == 0 && !mirror || want == -1 {
			return nil
		}
		return fmt.Errorf("missing AVIF %s transform", tag)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n != want {
		return fmt.Errorf("AVIF %s transform changed", tag)
	}
	return nil
}
func readMetadata(ctx context.Context, path string, run toolRunner) (map[string]string, error) {
	out, err := run(ctx, "exiftool", "-q", "-q", "-json", "-n", "-MIMEType", "-ImageWidth", "-ImageHeight", "-Orientation", "-DateTimeOriginal", "-CreateDate", "-OffsetTimeOriginal", "-GPSLatitude", "-GPSLongitude", "-Error", path)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	var rows []map[string]any
	if err = decoder.Decode(&rows); err != nil || len(rows) != 1 {
		return nil, errors.New("invalid ExifTool JSON")
	}
	result := make(map[string]string)
	for key, value := range rows[0] {
		switch v := value.(type) {
		case string:
			result[key] = v
		case json.Number:
			result[key] = string(v)
		}
	}
	if result["Error"] != "" {
		return nil, fmt.Errorf("ExifTool: %s", result["Error"])
	}
	return result, nil
}

// Metadata and decoder output are small. Cap them so a malformed file cannot
// exhaust coordinator memory; ExifTool's binary profile is capped as well.
func runTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out cappedBuffer
	cmd.Stdout = &out
	var stderr cappedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	if out.overflow {
		return nil, fmt.Errorf("%s output exceeds limit", name)
	}
	return out.Bytes(), nil
}

type cappedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (16 << 20) - b.Len()
	if len(p) > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
