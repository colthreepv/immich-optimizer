package main

import "testing"

func TestParseByteSize(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int64
	}{
		{"0", 0}, {"010", 10}, {"+010", 10}, {"01.5KiB", 1536},
		{"107374182400", 100 << 30}, {"100GiB", 100 << 30},
		{" 100 gib ", 100 << 30}, {"100 GiB", 100 << 30},
		{"1K", 1 << 10}, {"2M", 2 << 20}, {"3G", 3 << 30}, {"1T", 1 << 40},
		{"1KiB", 1 << 10}, {"2MiB", 2 << 20}, {"1TiB", 1 << 40},
		{"100GB", 100000000000}, {"1KB", 1000}, {"1MB", 1000000}, {"1TB", 1000000000000},
		{"1.5GiB", 3 << 29}, {"0.001KB", 1}, {"42B", 42},
		{"9223372036854775807", 9223372036854775807},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseByteSize(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("parseByteSize(%q) = %d, %v; want %d", tc.input, got, err, tc.want)
			}
		})
	}
	for _, value := range []string{"", "-1", "1e3", "1XB", "1GiBB", "NaN", "0.1KiB", "0.5B", "9223372036854775808", "8388608TiB", "1.2.3GiB"} {
		t.Run("reject "+value, func(t *testing.T) {
			if _, err := parseByteSize(value); err == nil {
				t.Fatalf("accepted invalid size %q", value)
			}
		})
	}
}

func TestParseConfigReadableSpoolLimit(t *testing.T) {
	t.Setenv("IUO_QUEUE_MAX_SPOOL_BYTES", "100GiB")
	tasks := writeTestTasksFile(t)
	args := []string{"-mode", "proxy", "-immich_url", "http://immich.example", "-tasks_file", tasks}
	config, _, err := parseConfig(args)
	if err != nil || config.QueueMaxSpoolBytes != 100<<30 {
		t.Fatalf("environment size: config=%+v err=%v", config, err)
	}
	config, _, err = parseConfig(append(args, "-queue_max_spool_bytes", "1.5GiB"))
	if err != nil || config.QueueMaxSpoolBytes != 3<<29 {
		t.Fatalf("flag override: config=%+v err=%v", config, err)
	}
	config, _, err = parseConfig(append(args, "-queue_max_spool_bytes", "0"))
	if err != nil || config.QueueMaxSpoolBytes != 0 {
		t.Fatalf("flag default sentinel: config=%+v err=%v", config, err)
	}
	if _, _, err = parseConfig(append(args, "-queue_max_spool_bytes", "8388608TiB")); err == nil {
		t.Fatal("flag overflow accepted")
	}
}

func TestOptionalByteSizeEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{"", 0}, {"1024", 1024}, {"100GiB", 100 << 30},
	} {
		t.Setenv("IUO_TEST_BYTE_SIZE", tc.value)
		got, err := optionalByteSizeEnv("IUO_TEST_BYTE_SIZE")
		if err != nil || got != tc.want {
			t.Fatalf("value %q: got %d, %v", tc.value, got, err)
		}
	}
	for _, value := range []string{"0", "-1GiB", "0.5B", "unknown"} {
		t.Setenv("IUO_TEST_BYTE_SIZE", value)
		if _, err := optionalByteSizeEnv("IUO_TEST_BYTE_SIZE"); err == nil {
			t.Fatalf("accepted environment size %q", value)
		}
	}
}
