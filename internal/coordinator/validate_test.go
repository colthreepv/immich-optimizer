package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateAVIF(t *testing.T) {
	base := map[string]any{"MIMEType": "image/jpeg", "ImageWidth": 4000, "ImageHeight": 3000, "Orientation": 6, "DateTimeOriginal": "2020:01:02 03:04:05", "CreateDate": "2020:01:02 03:04:05", "OffsetTimeOriginal": "+07:00", "GPSLatitude": 12.5, "GPSLongitude": 100.2}
	for _, tc := range []struct {
		name    string
		mutate  func(map[string]any)
		info    string
		icc     string
		wantErr string
	}{
		{"preserved", nil, "", "profile", ""},
		{"GPS lost", func(m map[string]any) { delete(m, "GPSLatitude") }, "", "profile", "GPSLatitude"},
		{"date changed", func(m map[string]any) { m["DateTimeOriginal"] = "different" }, "", "profile", "DateTimeOriginal"},
		{"create date lost", func(m map[string]any) { delete(m, "CreateDate") }, "", "profile", "CreateDate"},
		{"ICC lost", nil, "", "", "ICC"},
		{"dimensions changed", nil, " * Resolution : 3999x3000\n", "profile", "dimensions"},
		{"wrong MIME", func(m map[string]any) { m["MIMEType"] = "image/jpeg" }, "", "profile", "AVIF"},
		{"missing rotation", nil, " * Resolution : 4000x3000\n", "profile", "transform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := make(map[string]any)
			for k, v := range base {
				out[k] = v
			}
			out["MIMEType"] = "image/avif"
			if tc.mutate != nil {
				tc.mutate(out)
			}
			info := tc.info
			if info == "" {
				info = " * Resolution : 4000x3000\n * irot (Rotation) : 3\n"
			}
			info += " * Format : YUV420\n * Color Primaries : 1\n * Transfer Char. : 13\n * Matrix Coeffs : 6\n"
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "avifdec" {
					return []byte(info), nil
				}
				if name != "exiftool" {
					return nil, errors.New("unknown tool")
				}
				path := args[len(args)-1]
				if strings.Contains(strings.Join(args, " "), "-ICC_Profile") {
					if path == "source" {
						return []byte("profile"), nil
					}
					return []byte(tc.icc), nil
				}
				data := base
				if path == "result" {
					data = out
				}
				return json.Marshal([]map[string]any{data})
			}
			err := validateAVIF(context.Background(), "source", "result", run)
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("expected %s, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestValidateAVIFRealTools is opt-in because it requires the deployed image's
// encoder, decoder, ExifTool, ffmpeg and conversion script on PATH.
func TestValidateAVIFRealTools(t *testing.T) {
	if os.Getenv("IUO_TEST_IMAGE_TOOLS") != "1" {
		t.Skip("set IUO_TEST_IMAGE_TOOLS=1 to exercise the image tools")
	}
	for _, orientation := range []int{1, 6} {
		t.Run(fmt.Sprintf("orientation-%d", orientation), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			dir := t.TempDir()
			source, result := filepath.Join(dir, "source.jpg"), filepath.Join(dir, "result.avif")
			run := func(name string, args ...string) {
				t.Helper()
				cmd := exec.CommandContext(ctx, name, args...)
				cmd.Env = append(os.Environ(), "QUIET_EXEC_SENSOR_ENABLED=0")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s failed: %v\n%s", name, err, output)
				}
			}
			run("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=32x24:rate=1", "-frames:v", "1", "-threads", "1", "-y", source)
			run("exiftool", "-overwrite_original", "-DateTimeOriginal=2020:01:02 03:04:05", "-CreateDate=2020:01:02 03:04:05", "-OffsetTimeOriginal=+07:00", "-GPSLatitude=12.5", "-GPSLatitudeRef=N", "-GPSLongitude=100.2", "-GPSLongitudeRef=E", fmt.Sprintf("-Orientation#=%d", orientation), source)
			metadata, err := readMetadata(ctx, source, runTool)
			if err != nil {
				t.Fatal(err)
			}
			for _, tag := range []string{"DateTimeOriginal", "CreateDate", "OffsetTimeOriginal", "GPSLatitude", "GPSLongitude"} {
				if metadata[tag] == "" {
					t.Fatalf("fixture missing %s", tag)
				}
			}
			if metadata["Orientation"] != fmt.Sprint(orientation) {
				t.Fatalf("fixture orientation: %q", metadata["Orientation"])
			}
			run("jpeg-to-avif.sh", source, result)
			if err := ValidateAVIF(ctx, source, result); err != nil {
				info, infoErr := exec.CommandContext(ctx, "avifdec", "-i", result).CombinedOutput()
				t.Fatalf("validate converted image: %v\navifdec output (%v):\n%s", err, infoErr, info)
			}
		})
	}
}
func TestValidateAVIFToolFailures(t *testing.T) {
	run := func(context.Context, string, ...string) ([]byte, error) { return nil, fmt.Errorf("tool failed") }
	if err := validateAVIF(context.Background(), "source", "result", run); err == nil {
		t.Fatal("tool failure accepted")
	}
}

func TestValidateAVIFColorSignalLabels(t *testing.T) {
	for _, tc := range []struct {
		label string
		valid bool
	}{{"Matrix Coeffs", true}, {"Matrix Coeffs.", true}, {"Other", false}} {
		t.Run(tc.label, func(t *testing.T) {
			run := func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "avifdec" {
					return []byte(" * Resolution : 32x24\n * Format : YUV420\n * Color Primaries : 1\n * Transfer Char. : 13\n * " + tc.label + " : 6\n"), nil
				}
				if strings.Contains(strings.Join(args, " "), "-ICC_Profile") {
					return nil, nil
				}
				mime := "image/jpeg"
				if args[len(args)-1] == "result" {
					mime = "image/avif"
				}
				return json.Marshal([]map[string]any{{"MIMEType": mime, "ImageWidth": 32, "ImageHeight": 24}})
			}
			err := validateAVIF(context.Background(), "source", "result", run)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestContainerTransforms(t *testing.T) {
	for _, tc := range []struct {
		info             string
		rotation, mirror int
		valid            bool
	}{
		{"", 0, -1, true},
		{" * irot (Rotation) : 3\n", 3, -1, true},
		{" * irot (Rotation) : 1\n * imir (Mirror) : 0 (axis)\n", 1, 0, true},
		{" * irot (Rotation) : 1\n", 1, 0, false},
		{" * imir (Mirror) : 1 (axis)\n", 0, -1, false},
	} {
		err1 := checkTransform(tc.info, `irot \(Rotation\)`, tc.rotation, false)
		err2 := checkTransform(tc.info, `imir \(Mirror\)`, tc.mirror, true)
		if (err1 == nil && err2 == nil) != tc.valid {
			t.Fatalf("info %q: rotation=%v mirror=%v", tc.info, err1, err2)
		}
	}
}
