package main

import (
	"fmt"
	"math/big"
	"os"
	"regexp"
	"strings"
)

var byteSizePattern = regexp.MustCompile(`^\+?(\d+(?:\.\d+)?)[ \t]*([a-zA-Z]*)$`)

// Bare K/M/G/T follow Docker's binary units. Explicit KB/MB/GB/TB are decimal;
// KiB/MiB/GiB/TiB are binary. Rational arithmetic keeps whole-byte values exact.
func parseByteSize(value string) (int64, error) {
	parts := byteSizePattern.FindStringSubmatch(strings.TrimSpace(value))
	if parts == nil {
		return 0, fmt.Errorf("invalid byte size %q: use bytes or a unit such as 100GiB", value)
	}
	units := map[string]int64{
		"": 1, "B": 1,
		"K": 1 << 10, "KIB": 1 << 10, "KB": 1000,
		"M": 1 << 20, "MIB": 1 << 20, "MB": 1000 * 1000,
		"G": 1 << 30, "GIB": 1 << 30, "GB": 1000 * 1000 * 1000,
		"T": 1 << 40, "TIB": 1 << 40, "TB": 1000 * 1000 * 1000 * 1000,
	}
	multiplier, ok := units[strings.ToUpper(parts[2])]
	if !ok {
		return 0, fmt.Errorf("unknown byte size unit %q", parts[2])
	}
	whole, fraction, _ := strings.Cut(parts[1], ".")
	numerator, _ := new(big.Int).SetString(whole+fraction, 10)
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fraction))), nil)
	number := new(big.Rat).SetFrac(numerator, denominator)
	number.Mul(number, big.NewRat(multiplier, 1))
	if !number.IsInt() || !number.Num().IsInt64() {
		return 0, fmt.Errorf("byte size %q must resolve to whole bytes within int64 range", value)
	}
	return number.Num().Int64(), nil
}

func optionalByteSizeEnv(key string) (int64, error) {
	value := os.Getenv(key)
	if value == "" {
		return 0, nil
	}
	parsed, err := parseByteSize(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if parsed == 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return parsed, nil
}
