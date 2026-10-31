package replacement

import (
	"context"
	"errors"
	"testing"
)

func TestEquivalentCaptureTimeZone(t *testing.T) {
	for _, tc := range []struct {
		name, source, target, date string
		want                       bool
	}{
		{"observed Bangkok", "UTC+7", "Asia/Bangkok", "2025-02-15T21:59:28.325+07:00", true},
		{"padded offset", "UTC+07:00", "Asia/Bangkok", "2025-02-15T21:59:28+07:00", true},
		{"fractional offset", "UTC+5:45", "Asia/Kathmandu", "2025-02-15T21:59:28+05:45", true},
		{"negative offset", "UTC-5", "America/New_York", "2025-02-15T21:59:28-05:00", true},
		{"DST summer", "UTC-4", "America/New_York", "2025-07-15T21:59:28-04:00", true},
		{"UTC", "UTC", "Etc/UTC", "2025-02-15T21:59:28Z", true},
		{"wrong zone", "UTC+7", "Asia/Tokyo", "2025-02-15T21:59:28+07:00", false},
		{"wrong timestamp offset", "UTC+7", "Asia/Bangkok", "2025-02-15T21:59:28Z", false},
		{"wrong DST season", "UTC-5", "America/New_York", "2025-07-15T21:59:28-05:00", false},
		{"distinct IANA zones", "Europe/Rome", "Europe/Berlin", "2025-02-15T21:59:28+01:00", false},
		{"IANA loss", "Asia/Bangkok", "UTC+7", "2025-02-15T21:59:28+07:00", false},
		{"unknown target", "UTC+7", "Invalid/Zone", "2025-02-15T21:59:28+07:00", false},
		{"machine local", "UTC+7", "Local", "2025-02-15T21:59:28+07:00", false},
		{"invalid fixed zone", "UTC+7:99", "Asia/Bangkok", "2025-02-15T21:59:28+07:00", false},
		{"invalid date", "UTC+7", "Asia/Bangkok", "2025-02-15T21:59:28", false},
		{"missing date", "UTC+7", "Asia/Bangkok", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := map[string]any{"timeZone": tc.source, "dateTimeOriginal": tc.date}
			target := map[string]any{"timeZone": tc.target, "dateTimeOriginal": tc.date}
			if got := equivalentCaptureTimeZone(source, target); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("date changed", func(t *testing.T) {
		source := map[string]any{"timeZone": "UTC+7", "dateTimeOriginal": "2025-02-15T21:59:28+07:00"}
		target := map[string]any{"timeZone": "Asia/Bangkok", "dateTimeOriginal": "2025-02-15T22:59:28+07:00"}
		if equivalentCaptureTimeZone(source, target) {
			t.Fatal("changed date accepted")
		}
	})
}

func TestPublishAndRetireWithNormalizedCaptureTimeZone(t *testing.T) {
	for _, target := range []string{"Asia/Bangkok", "Asia/Tokyo", "Europe/Berlin"} {
		t.Run(target, func(t *testing.T) {
			f, c := newFixture(t)
			date := "2025-02-15T21:59:28.325+07:00"
			f.sourceExif = map[string]any{"timeZone": "UTC+7", "dateTimeOriginal": date}
			f.outputExif = map[string]any{"timeZone": target, "dateTimeOriginal": date}
			r, err := c.Publish(context.Background(), f.input, f.checkpoint)
			if target != "Asia/Bangkok" {
				if !errors.Is(err, ErrBlocked) || f.wasTrashed() {
					t.Fatalf("unsafe timezone migration: err=%v trashed=%v", err, f.wasTrashed())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = c.Retire(context.Background(), f.input, r, true); err != nil || !f.wasTrashed() {
				t.Fatalf("equivalent timezone migration blocked: err=%v trashed=%v", err, f.wasTrashed())
			}
		})
	}
}
