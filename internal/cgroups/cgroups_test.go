package cgroups

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A fake cgroup root whose host offers cpu and pids but not memory, as when
// memory is still on cgroup v1.
func TestCreateEnablesOnlyOfferedControllers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpuset cpu io pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cg, err := create(root, "c1", Limits{CPUs: 1, Pids: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, filepath.Join(root, "microbox")} {
		if got := readFile(t, filepath.Join(dir, "cgroup.subtree_control")); got != "+cpu +pids" {
			t.Errorf("%s/cgroup.subtree_control = %q, want +cpu +pids", dir, got)
		}
	}
	if got := readFile(t, filepath.Join(cg.Path, "cpu.max")); got != "100000 100000" {
		t.Errorf("cpu.max = %q", got)
	}

	_, err = create(root, "c2", Limits{MemoryBytes: 64 << 20})
	if err == nil || !strings.Contains(err.Error(), "memory controller") {
		t.Errorf("a memory limit without the memory controller: err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "microbox", "c2")); !os.IsNotExist(err) {
		t.Error("the group was created even though its limit can't be set")
	}

	if _, err := create(t.TempDir(), "c3", Limits{}); err == nil {
		t.Error("create succeeded without cgroup.controllers")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

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
