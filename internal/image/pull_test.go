package image

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/anishmehta24/microbox/internal/image/imagetest"
)

// Unpacking keeps owners, creates device nodes and sets trusted.* xattrs,
// all of which need root.
func needRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	Dir = t.TempDir()
}

var (
	base = []imagetest.Entry{
		{Name: "etc/", Type: tar.TypeDir},
		{Name: "etc/motd", Body: "welcome\n"},
		{Name: "opt/", Type: tar.TypeDir},
		{Name: "opt/a", Body: "from the base layer\n"},
	}
	top = []imagetest.Entry{
		{Name: "etc/.wh.motd"},     // deletes etc/motd
		{Name: "opt/.wh..wh..opq"}, // hides opt/a
		{Name: "opt/b", Body: "from the top layer\n"},
		{Name: "hello.txt", Body: "hello\n", Uid: 1000, Gid: 1000},
		{Name: "hard", Type: tar.TypeLink, Linkname: "hello.txt"},
		{Name: "soft", Type: tar.TypeSymlink, Linkname: "/hello.txt"},
		{Name: "suid", Body: "#!/bin/sh\n", Mode: 0o4755},
		{Name: "null", Type: tar.TypeChar, Mode: 0o666},
	}
	config = Config{Cmd: []string{"cat", "/hello.txt"}, Env: []string{"A=1"}, WorkingDir: "/opt", User: "1000"}
)

func TestPull(t *testing.T) {
	needRoot(t)
	reg := imagetest.NewRegistry(t)
	reg.Add("team/app", "v1", config, imagetest.Layer(t, base...), imagetest.Layer(t, top...))
	ref := reg.Host() + "/team/app:v1"

	img, err := Pull(context.Background(), ref, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if img.Ref != ref || len(img.Layers) != 2 || img.Config.User != "1000" || img.Config.Cmd[0] != "cat" {
		t.Errorf("record: %+v", img)
	}
	if got, err := Lookup(ref); err != nil || got.ID != img.ID {
		t.Errorf("Lookup = %+v, %v", got, err)
	}
	upper := img.LowerDirs()[0]

	// The whiteout is a 0:0 character device, the opaque directory has the xattr.
	var st unix.Stat_t
	if err := unix.Lstat(filepath.Join(upper, "etc/motd"), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR || st.Rdev != 0 {
		t.Errorf("etc/motd in the top layer: mode %o rdev %d, %v; want a 0:0 char device", st.Mode, st.Rdev, err)
	}
	buf := make([]byte, 8)
	if n, err := unix.Lgetxattr(filepath.Join(upper, "opt"), "trusted.overlay.opaque", buf); err != nil || string(buf[:n]) != "y" {
		t.Errorf("opt isn't opaque: %q, %v", buf[:n], err)
	}
	var hello, hard unix.Stat_t
	unix.Lstat(filepath.Join(upper, "hello.txt"), &hello)
	unix.Lstat(filepath.Join(upper, "hard"), &hard)
	if hello.Ino != hard.Ino || hello.Uid != 1000 || hello.Gid != 1000 {
		t.Errorf("hello.txt: inode %d uid %d gid %d; hard: inode %d", hello.Ino, hello.Uid, hello.Gid, hard.Ino)
	}
	if fi, err := os.Lstat(filepath.Join(upper, "suid")); err != nil || fi.Mode()&os.ModeSetuid == 0 {
		t.Errorf("suid lost its setuid bit: %v, %v", fi.Mode(), err)
	}
	if target, err := os.Readlink(filepath.Join(upper, "soft")); err != nil || target != "/hello.txt" {
		t.Errorf("soft -> %q, %v", target, err)
	}

	// What overlayfs makes of the two layers.
	merged := t.TempDir()
	if err := syscall.Mount("overlay", merged, "overlay", syscall.MS_RDONLY, "lowerdir="+strings.Join(img.LowerDirs(), ":")); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount(merged, 0)
	for path, want := range map[string]bool{"etc/motd": false, "opt/a": false, "opt/b": true, "hello.txt": true, "etc": true} {
		if _, err := os.Stat(filepath.Join(merged, path)); (err == nil) != want {
			t.Errorf("%s in the overlay: %v, want present=%v", path, err, want)
		}
	}

	// Pulling again finds the tag unchanged with a HEAD request.
	blobs := reg.Count("GET /v2/team/app/blobs/")
	if _, err := Pull(context.Background(), ref, io.Discard); err != nil {
		t.Fatal(err)
	}
	if n := reg.Count("GET /v2/team/app/"); n != blobs+2 { // the two manifests the first pull fetched
		t.Errorf("the second pull made %d more GET requests, want none:\n%s", n-blobs-2, strings.Join(reg.Requests, "\n"))
	}
}

func TestPullRejectsCorruptLayer(t *testing.T) {
	needRoot(t)
	reg := imagetest.NewRegistry(t)
	layers := reg.Add("app", "v1", config, imagetest.Layer(t, base...))
	reg.Corrupt(layers[0])
	_, err := Pull(context.Background(), reg.Host()+"/app:v1", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "content has digest") {
		t.Fatalf("err = %v, want a digest mismatch", err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(Dir, "*", "*"))
	if len(leftovers) != 0 {
		t.Errorf("left behind: %v", leftovers)
	}
}

func TestPullRejectsPathsOutsideTheLayer(t *testing.T) {
	needRoot(t)
	outside := t.TempDir()
	for name, entries := range map[string][]imagetest.Entry{
		"dot-dot": {{Name: "../../escape", Body: "x"}},
		"through a symlink": {
			{Name: "evil", Type: tar.TypeSymlink, Linkname: outside},
			{Name: "evil/escape", Body: "x"},
		},
		"hard link out": {{Name: "passwd", Type: tar.TypeLink, Linkname: "../../../etc/passwd"}},
	} {
		reg := imagetest.NewRegistry(t)
		reg.Add("app", "v1", config, imagetest.Layer(t, entries...))
		if _, err := Pull(context.Background(), reg.Host()+"/app:v1", io.Discard); err == nil {
			t.Errorf("%s: pull succeeded", name)
		}
		if _, err := os.Stat(filepath.Join(outside, "escape")); err == nil {
			t.Fatalf("%s: wrote outside the layer", name)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(Dir), "escape")); err == nil {
		t.Error("wrote next to the store")
	}
}

func TestRemoveUnusedLayers(t *testing.T) {
	needRoot(t)
	reg := imagetest.NewRegistry(t)
	reg.Add("app", "v1", config, imagetest.Layer(t, base...), imagetest.Layer(t, top...))
	img, err := Pull(context.Background(), reg.Host()+"/app:v1", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := RemoveUnusedLayers(nil); err != nil || len(removed) != 0 {
		t.Errorf("removed %v, %v while the image still exists", removed, err)
	}
	if err := Remove(img.Ref); err != nil {
		t.Fatal(err)
	}
	// A container still uses the top layer.
	removed, err := RemoveUnusedLayers(img.LowerDirs()[:1])
	if err != nil || len(removed) != 1 || removed[0] != strings.TrimPrefix(img.Layers[0], "sha256:") {
		t.Errorf("removed %v, %v; want only the base layer", removed, err)
	}
}
