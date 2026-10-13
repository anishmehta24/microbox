package container

import (
	"os"
	"path/filepath"
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
	if got, err := resolveImage(ok); err != nil || got != ok {
		t.Errorf("resolveImage(%q) = %q, %v", ok, got, err)
	}
}
