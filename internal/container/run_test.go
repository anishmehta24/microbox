package container

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveImageRejectsOverlaySpecialChars(t *testing.T) {
	for _, name := range []string{`a,b`, `a:b`, `a\b`} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveImage(dir); err == nil || !strings.Contains(err.Error(), "special meaning") {
			t.Errorf("resolveImage(%q) = %v, want a rejection", dir, err)
		}
	}
	ok := filepath.Join(t.TempDir(), "rootfs")
	if err := os.Mkdir(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveImage(ok); err != nil || !reflect.DeepEqual(got.lower, []string{ok}) {
		t.Errorf("resolveImage(%q) = %v, %v", ok, got, err)
	}
}

func TestContainerEnv(t *testing.T) {
	t.Setenv("TERM", "xterm")
	got := containerEnv([]string{"PATH=/opt/bin:/bin", "GREETING=hi", "HOSTNAME=image"}, "box")
	want := []string{"PATH=/opt/bin:/bin", "GREETING=hi", "HOSTNAME=box", "TERM=xterm"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("containerEnv = %q\nwant %q", got, want)
	}
	if got := containerEnv(nil, "box"); !strings.HasPrefix(got[0], "PATH=/usr/local/sbin:") {
		t.Errorf("no default PATH: %q", got)
	}
}
