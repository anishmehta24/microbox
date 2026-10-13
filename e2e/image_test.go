package e2e

import (
	"net"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/anishmehta24/microbox/internal/container"
	"github.com/anishmehta24/microbox/internal/image"
	"github.com/anishmehta24/microbox/internal/image/imagetest"
)

// A three-layer image from a registry on 127.0.0.1: alpine, a layer that adds
// /opt/a, and one that deletes /etc/motd, hides /opt/a behind an opaque /opt
// with /opt/b in it, and adds /hello.txt. The config sets the command,
// environment, working directory and user.
func layeredImage(t *testing.T) (*imagetest.Registry, string) {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
	reg := imagetest.NewRegistry(t)
	reg.Add("test/layered", "v1",
		image.Config{
			Env:        []string{"GREETING=hi"},
			Cmd:        []string{"sh", "-c", `echo "$GREETING from $(pwd) as $(id -u) with HOME=$HOME"; cat /hello.txt; ls /opt; test -e /etc/motd || echo no motd`},
			WorkingDir: "/opt",
			User:       "1000:1000",
		},
		imagetest.DirLayer(t, filepath.Join(container.ImagesDir, "alpine")),
		imagetest.Layer(t, imagetest.Entry{Name: "opt/a", Body: "a\n"}),
		imagetest.Layer(t,
			imagetest.Entry{Name: "etc/.wh.motd"},
			imagetest.Entry{Name: "opt/.wh..wh..opq"},
			imagetest.Entry{Name: "opt/b", Body: "b\n"},
			imagetest.Entry{Name: "hello.txt", Body: "hello\n"},
		),
	)
	return reg, reg.Host() + "/test/layered:v1"
}

func TestPullAndRun(t *testing.T) {
	reg, ref := layeredImage(t)
	t.Cleanup(func() { cli(t, "rmi", ref); cli(t, "gc") })
	if out := mustCLI(t, "pull", ref); !strings.HasSuffix(out, "pulled "+ref+" ("+imageID(t, ref)+")") {
		t.Errorf("pull printed:\n%s", out)
	}
	if out := mustCLI(t, "images"); !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(ref) + `\s+` + imageID(t, ref) + `\s+3\s`).MatchString(out) {
		t.Errorf("images doesn't list it with 3 layers:\n%s", out)
	}

	// The image's command, environment, working directory and user, and the
	// layers stacked with the deletions applied.
	want := "hi from /opt as 1000 with HOME=/\nhello\nb\nno motd"
	if got := mustRun(t, ref); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// A command replaces the image's; --user replaces its user.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{ref, "id", "-u"}, "1000"},
		{[]string{"--user", "root", ref, "sh", "-c", "id -u; echo $HOME"}, "0\n/root"},
		{[]string{"--user", "nobody", ref, "id", "-u"}, "65534"},
		{[]string{"-u", "bin:daemon", ref, "sh", "-c", "id -u; id -g"}, "1\n2"},
	} {
		if got := mustRun(t, tc.args...); got != tc.want {
			t.Errorf("run %v: %q, want %q", tc.args, got, tc.want)
		}
	}
	if out, code := microbox(t, "--user", "nosuchuser", ref, "true"); code != 125 || !strings.Contains(out, `user "nosuchuser" isn't in the image's /etc/passwd`) {
		t.Errorf("unknown user: exit %d\n%s", code, out)
	}

	// Pulling again sends only HEAD requests for the manifest.
	gets := reg.Count("GET ")
	if out := mustCLI(t, "pull", ref); out != ref+" is up to date" {
		t.Errorf("second pull printed %q", out)
	}
	if n := reg.Count("GET "); n != gets {
		t.Errorf("the second pull made %d GET requests", n-gets)
	}

	// rmi forgets the image, gc deletes its layers.
	mustCLI(t, "rmi", ref)
	if out := mustCLI(t, "images"); strings.Contains(out, ref) {
		t.Errorf("images still lists it:\n%s", out)
	}
	if out := mustCLI(t, "gc"); strings.Count(out, "removed layer ") != 3 {
		t.Errorf("gc printed:\n%s", out)
	}
}

func imageID(t *testing.T, ref string) string {
	t.Helper()
	img, err := image.Lookup(ref)
	if err != nil {
		return "(not pulled)"
	}
	return strings.TrimPrefix(img.ID, "sha256:")[:12]
}

// A container keeps its layers after rmi of its image, until it is removed.
func TestLayersOutliveImageWhileUsed(t *testing.T) {
	_, ref := layeredImage(t)
	mustCLI(t, "pull", ref)
	id := runDetached(t, ref, "sleep", "60")
	mustCLI(t, "rmi", ref)
	if out := mustCLI(t, "gc"); strings.Contains(out, "removed layer") {
		t.Errorf("gc removed layers of a running container:\n%s", out)
	}
	if got := mustCLI(t, "exec", id, "cat", "/hello.txt"); got != "hello" {
		t.Errorf("exec after rmi: %q", got)
	}
	mustCLI(t, "rm", "-f", id)
	if out := mustCLI(t, "gc"); strings.Count(out, "removed layer ") != 3 {
		t.Errorf("gc after rm printed:\n%s", out)
	}
}

// The real thing, when the host is online. The image stays pulled, so that
// later runs only make a HEAD request: Docker Hub limits anonymous pulls.
func TestPullFromDockerHub(t *testing.T) {
	if skipReason != "" {
		t.Skip(skipReason)
	}
	if c, err := net.DialTimeout("tcp", "registry-1.docker.io:443", 5*time.Second); err != nil {
		t.Skipf("Docker Hub isn't reachable: %v", err)
	} else {
		c.Close()
	}
	const ref = "busybox:1.36"
	mustCLI(t, "pull", ref)
	if got := mustRun(t, ref, "sh", "-c", "busybox | head -1"); !strings.HasPrefix(got, "BusyBox v1.36") {
		t.Errorf("got %q", got)
	}
}
