package replacement

import (
	"regexp"
	"strconv"
	"time"
	_ "time/tzdata" // Keep IANA validation independent of the container's zoneinfo files.
)

var fixedCaptureZone = regexp.MustCompile(`^UTC([+-])(\d{1,2})(?::([0-5]\d))?$`)

// Immich can normalize a JPEG's fixed-offset label to an IANA zone on AVIF
// extraction. Accept that upgrade only when the unchanged capture timestamp's
// offset matches both labels. Distinct source IANA zones remain significant.
func equivalentCaptureTimeZone(source, target map[string]any) bool {
	sourceZone, ok := source["timeZone"].(string)
	if !ok {
		return false
	}
	seconds := 0
	if sourceZone != "UTC" {
		m := fixedCaptureZone.FindStringSubmatch(sourceZone)
		if m == nil {
			return false
		}
		hours, _ := strconv.Atoi(m[2])
		minutes, _ := strconv.Atoi(m[3])
		if hours > 14 || (hours == 14 && minutes != 0) {
			return false
		}
		seconds = hours*3600 + minutes*60
		if m[1] == "-" {
			seconds = -seconds
		}
	}
	date, ok := source["dateTimeOriginal"].(string)
	if !ok || date == "" || target["dateTimeOriginal"] != date {
		return false
	}
	capture, err := time.Parse(time.RFC3339Nano, date)
	if err != nil {
		return false
	}
	_, offset := capture.Zone()
	if offset != seconds {
		return false
	}
	targetZone, ok := target["timeZone"].(string)
	if !ok || targetZone == "" || targetZone == "Local" {
		return false
	}
	location, err := time.LoadLocation(targetZone)
	if err != nil {
		return false
	}
	_, offset = capture.In(location).Zone()
	return offset == seconds
}
