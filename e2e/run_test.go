// Package e2e runs the real microbox binary against the alpine image. The tests
// need root and the image (sudo scripts/get-alpine.sh) and skip without them.
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anishmehta24/microbox/internal/container"
)

var (
	bin        string // path to the microbox binary built by TestMain
	skipReason string
)

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		skipReason = "needs root"
	} else if _, err := os.Stat(filepath.Join(container.ImagesDir, "alpine", "etc", "alpine-release")); err != nil {
		skipReason = "alpine image missing (sudo scripts/get-alpine.sh)"
	}
	if skipReason != "" {
		os.Exit(m.Run())
	}

	dir, err := os.MkdirTemp("", "microbox-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "microbox")
	build := exec.Command("go", "build", "-o", bin, "../cmd/microbox")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build microbox:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// microbox runs `microbox run ARGS...` and returns its combined output and exit code.
func microbox(t *testing.T, args ...string) (string, int) {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
	var out bytes.Buffer
	cmd := exec.Command(bin, append([]string{"run"}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil && cmd.ProcessState == nil {
		t.Fatalf("start microbox: %v", err)
	}
	return strings.TrimSpace(out.String()), cmd.ProcessState.ExitCode()
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, code := microbox(t, args...)
	if code != 0 {
		t.Fatalf("microbox run %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

func TestHostname(t *testing.T) {
	before, _ := os.Hostname()
	if got := mustRun(t, "--hostname", "box1", "alpine", "hostname"); got != "box1" {
		t.Errorf("hostname = %q, want box1", got)
	}
	if after, _ := os.Hostname(); after != before {
		t.Errorf("host hostname changed from %q to %q", before, after)
	}
}

func TestDefaultHostnameIsID(t *testing.T) {
	got := mustRun(t, "alpine", "hostname")
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(got) {
		t.Errorf("hostname = %q, want a 12-char hex ID", got)
	}
}

func TestCommandIsPID1(t *testing.T) {
	if got := mustRun(t, "alpine", "sh", "-c", "echo $$"); got != "1" {
		t.Errorf("shell PID = %q, want 1", got)
	}
}

func TestOnlyContainerProcessesVisible(t *testing.T) {
	out := mustRun(t, "alpine", "ps", "-o", "pid,comm")
	lines := strings.Split(out, "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[1]), " ") != "1 ps" {
		t.Errorf("ps inside the container should only see itself as PID 1, got:\n%s", out)
	}
}

func TestNamespacesDiffer(t *testing.T) {
	out := mustRun(t, "alpine", "sh", "-c", "for n in uts pid mnt ipc; do readlink /proc/self/ns/$n; done")
	inside := strings.Split(out, "\n")
	for i, ns := range []string{"uts", "pid", "mnt", "ipc"} {
		host, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil {
			t.Fatal(err)
		}
		if i >= len(inside) || inside[i] == host {
			t.Errorf("%s namespace not isolated: host %s, container %v", ns, host, inside)
		}
	}
}

func TestRootIsImage(t *testing.T) {
	if got := mustRun(t, "alpine", "cat", "/etc/alpine-release"); got == "" {
		t.Error("/etc/alpine-release is empty")
	}
	// Only the image itself and /proc: the host's mounts went away with the old root.
	mounts := strings.Split(mustRun(t, "alpine", "cat", "/proc/mounts"), "\n")
	if len(mounts) != 2 || !strings.HasPrefix(mounts[1], "proc /proc proc ") {
		t.Errorf("container mounts:\n%s\nwant just / and /proc", strings.Join(mounts, "\n"))
	}
}

func TestNoMountLeakToHost(t *testing.T) {
	before := countLines(t, "/proc/self/mountinfo")
	mustRun(t, "alpine", "true")
	if after := countLines(t, "/proc/self/mountinfo"); after != before {
		t.Errorf("host mount count went from %d to %d", before, after)
	}
}

func TestExitCodes(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"alpine", "sh", "-c", "exit 7"}, 7},
		{[]string{"alpine", "no-such-command"}, 127},
		{[]string{"alpine", "/etc/passwd"}, 126},
		{[]string{"no-such-image", "true"}, 125},
	} {
		if out, code := microbox(t, tc.args...); code != tc.want {
			t.Errorf("microbox run %s: exit %d, want %d\n%s", strings.Join(tc.args, " "), code, tc.want, out)
		}
	}
}

// The kernel protects a namespace's PID 1: signals it has no handler for are
// dropped when they come from inside the namespace, even SIGKILL.
func TestPID1IgnoresKillFromInside(t *testing.T) {
	if got := mustRun(t, "alpine", "sh", "-c", "kill -KILL $$; echo survived"); got != "survived" {
		t.Errorf("got %q, want the shell to survive its own SIGKILL", got)
	}
}

// From the parent namespace SIGKILL does get through, and microbox reports it
// as 128+9.
func TestKilledFromHostExits137(t *testing.T) {
	if skipReason != "" {
		t.Skip(skipReason)
	}
	cmd := exec.Command(bin, "run", "alpine", "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(childOf(t, cmd.Process.Pid), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 137 {
		t.Errorf("exit %d, want 137", code)
	}
}

// childOf polls /proc until the process ppid has a child and returns its PID.
func childOf(t *testing.T, ppid int) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		stats, _ := filepath.Glob("/proc/[0-9]*/stat")
		for _, path := range stats {
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			// "pid (comm) state ppid ...": comm may contain spaces, so split after the last ')'.
			fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
			if len(fields) > 1 && fields[1] == strconv.Itoa(ppid) {
				pid, _ := strconv.Atoi(filepath.Base(filepath.Dir(path)))
				return pid
			}
		}
	}
	t.Fatalf("process %d never got a child", ppid)
	return 0
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}
