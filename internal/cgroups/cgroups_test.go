package cgroups

import (
	"reflect"
	"testing"
)

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"1":     1,
		"4096":  4096,
		"512k":  512 << 10,
		"256m":  256 << 20,
		"256M":  256 << 20,
		"2g":    2 << 30,
		"1024K": 1 << 20,
	} {
		if got, err := ParseBytes(in); err != nil || got != want {
			t.Errorf("ParseBytes(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "m", "abc", "-1m", "0", "1.5g", "10x", "1t", "9999999999g"} {
		if got, err := ParseBytes(in); err == nil {
			t.Errorf("ParseBytes(%q) = %d, want an error", in, got)
		}
	}
}

func TestLimitFiles(t *testing.T) {
	if got := limitFiles(Limits{}); len(got) != 0 {
		t.Errorf("no limits wrote %v", got)
	}
	got := limitFiles(Limits{MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32})
	want := []limitFile{
		{"memory.max", "67108864"},
		{"memory.swap.max", "0"},
		{"cpu.max", "50000 100000"},
		{"pids.max", "32"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("limitFiles = %v\nwant %v", got, want)
	}
	// Rounded to the nearest microsecond, not truncated: 0.29*100000 is 28999.999... in floating point.
	if got := limitFiles(Limits{CPUs: 0.29}); got[0].value != "29000 100000" {
		t.Errorf("cpu.max for 0.29 CPUs = %q", got[0].value)
	}
}
