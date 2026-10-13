// Package e2e runs the real microbox binary against the alpine image. The tests
// need root and the image (sudo scripts/get-alpine.sh) and skip without them.
package e2e

import (
	"bytes"
	"context"
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

	"github.com/anishmehta24/microbox/internal/cgroups"
	"github.com/anishmehta24/microbox/internal/container"
	"github.com/anishmehta24/microbox/internal/network"
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // a hung container fails the test instead of the run
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append([]string{"run"}, args...)...)
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
	out := mustRun(t, "alpine", "sh", "-c", "for n in uts pid mnt ipc cgroup net; do readlink /proc/self/ns/$n; done")
	inside := strings.Split(out, "\n")
	for i, ns := range []string{"uts", "pid", "mnt", "ipc", "cgroup", "net"} {
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
}

// Exactly the overlay and the special filesystems: the host's mounts went away
// with the old root.
func TestMounts(t *testing.T) {
	want := []string{
		"/ overlay",
		"/proc proc",
		"/sys sysfs",
		"/sys/fs/cgroup cgroup2",
		"/dev tmpfs",
		"/dev/pts devpts",
		"/dev/shm tmpfs",
		"/dev/mqueue mqueue",
	}
	var got []string
	for _, line := range strings.Split(mustRun(t, "alpine", "cat", "/proc/mounts"), "\n") {
		f := strings.Fields(line) // source target fstype options ...
		got = append(got, f[1]+" "+f[2])
		if (f[1] == "/sys" || f[1] == "/sys/fs/cgroup") && !strings.HasPrefix(f[3], "ro,") {
			t.Errorf("%s is mounted %s, want read-only", f[1], f[3])
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("container mounts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// /dev holds the default devices and links and nothing from the host.
func TestDev(t *testing.T) {
	want := "fd full mqueue null ptmx pts random shm stderr stdin stdout tty urandom zero"
	if got := strings.Join(strings.Fields(mustRun(t, "alpine", "ls", "/dev")), " "); got != want {
		t.Errorf("ls /dev = %s\nwant      %s", got, want)
	}
	script := `for d in null zero full random urandom tty; do test -c /dev/$d || echo "/dev/$d is not a char device"; done
		echo discarded > /dev/null
		head -c 3 /dev/urandom | wc -c`
	if got := mustRun(t, "alpine", "sh", "-c", script); got != "3" {
		t.Errorf("got %q, want 3", got)
	}
}

func TestEtcFiles(t *testing.T) {
	if got := mustRun(t, "--hostname", "box2", "alpine", "cat", "/etc/hostname"); got != "box2" {
		t.Errorf("/etc/hostname = %q, want box2", got)
	}
	if got := mustRun(t, "--hostname", "box2", "--net", "none", "alpine", "grep", "box2", "/etc/hosts"); got != "127.0.1.1\tbox2" {
		t.Errorf("/etc/hosts line without an address = %q", got)
	}
	host, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Skip(err)
	}
	want := strings.TrimSpace(string(network.ResolvConf(host)))
	if got := mustRun(t, "alpine", "cat", "/etc/resolv.conf"); got != want {
		t.Errorf("/etc/resolv.conf = %q, want %q", got, want)
	}
	if got := mustRun(t, "--net", "host", "alpine", "cat", "/etc/resolv.conf"); got != strings.TrimSpace(string(host)) {
		t.Errorf("/etc/resolv.conf with --net host = %q, want the host's unchanged", got)
	}
}

// A container's writes, deletes included, land in its own layer: the image and
// the next container don't see them.
func TestWritesStayInContainer(t *testing.T) {
	image := filepath.Join(container.ImagesDir, "alpine")
	if got := mustRun(t, "alpine", "sh", "-c", "echo hi > /etc/marker && rm /bin/ls && cat /etc/marker"); got != "hi" {
		t.Fatalf("got %q, want hi", got)
	}
	if _, err := os.Lstat(filepath.Join(image, "etc", "marker")); err == nil {
		t.Error("the write reached the image")
	}
	if _, err := os.Lstat(filepath.Join(image, "bin", "ls")); err != nil {
		t.Errorf("the delete reached the image: %v", err)
	}
	if got := mustRun(t, "alpine", "sh", "-c", "test ! -e /etc/marker && test -e /bin/ls && echo clean"); got != "clean" {
		t.Errorf("next container saw the changes: %q", got)
	}
}

func TestParallelContainersHaveOwnLayers(t *testing.T) {
	t.Run("group", func(t *testing.T) {
		for _, v := range []string{"A", "B", "C"} {
			t.Run(v, func(t *testing.T) {
				t.Parallel()
				if got := mustRun(t, "alpine", "sh", "-c", "echo "+v+" > /shared; sleep 0.3; cat /shared"); got != v {
					t.Errorf("container %s read %q", v, got)
				}
			})
		}
	})
}

func TestContainerDirRemovedAfterExit(t *testing.T) {
	before := containerDirs(t)
	mustRun(t, "alpine", "sh", "-c", "echo data > /tmp/f")
	for name := range containerDirs(t) {
		if !before[name] {
			t.Errorf("leftover container directory %s", name)
		}
	}
}

func containerDirs(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(container.ContainersDir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for _, e := range entries {
		dirs[e.Name()] = true
	}
	return dirs
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

// The cgroup namespace starts at the container's own group.
func TestCgroupNamespace(t *testing.T) {
	if got := mustRun(t, "alpine", "cat", "/proc/self/cgroup"); got != "0::/" {
		t.Errorf("/proc/self/cgroup = %q, want 0::/", got)
	}
}

func TestLimitsVisibleInside(t *testing.T) {
	read := "cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max /sys/fs/cgroup/cpu.max /sys/fs/cgroup/pids.max"
	got := mustRun(t, "--memory", "64m", "--cpus", "0.5", "--pids", "32", "alpine", "sh", "-c", read)
	if want := "67108864\n0\n50000 100000\n32"; got != want {
		t.Errorf("limits inside:\n%s\nwant:\n%s", got, want)
	}
	got = mustRun(t, "alpine", "cat", "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/pids.max")
	if want := "max\nmax 100000\nmax"; got != want {
		t.Errorf("default limits:\n%s\nwant:\n%s", got, want)
	}
}

// The shell holds 100 MB in a variable, which a 32 MB container can't. The
// tr keeps the bytes non-zero, since the shell would drop NUL bytes.
func TestMemoryLimitOOMKills(t *testing.T) {
	out, code := microbox(t, "--memory", "32m", "alpine", "sh", "-c", `x=$(head -c 100m /dev/zero | tr '\0' a); echo survived`)
	if code != 128+9 || !strings.Contains(out, "OOM killer killed") || strings.Contains(out, "survived") {
		t.Errorf("exit %d, output:\n%s\nwant an OOM kill (exit 137)", code, out)
	}
}

// Forking in a subshell: when fork fails the subshell aborts, but PID 1 lives
// on to read the counters with shell builtins, which don't need a fork.
func TestPidsLimit(t *testing.T) {
	script := `(for i in $(seq 20); do sleep 2 & done) 2>/dev/null
		read peak < /sys/fs/cgroup/pids.peak
		read ev < /sys/fs/cgroup/pids.events
		echo "$peak $ev"`
	f := strings.Fields(mustRun(t, "--pids", "8", "alpine", "sh", "-c", script)) // "8 max N"
	if len(f) != 3 || f[0] != "8" || f[1] != "max" || f[2] == "0" {
		t.Errorf("got %v, want a peak of 8 processes and failed forks counted", f)
	}
}

// A busy loop for one second at 0.2 CPUs gets throttled to about 200 ms of CPU.
func TestCPULimit(t *testing.T) {
	out := mustRun(t, "--cpus", "0.2", "alpine", "sh", "-c", `timeout 1 sh -c 'while :; do :; done'; cat /sys/fs/cgroup/cpu.stat`)
	stat := map[string]int64{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			stat[k], _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if stat["nr_throttled"] == 0 || stat["usage_usec"] > 400000 {
		t.Errorf("usage_usec=%d nr_throttled=%d, want throttling well under 1 s of CPU\n%s", stat["usage_usec"], stat["nr_throttled"], out)
	}
}

func TestBadLimitFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--memory", "10x", "alpine", "true"},
		{"--cpus", "0.001", "alpine", "true"},
		{"--cpus", "NaN", "alpine", "true"},
		{"--cpus", "+Inf", "alpine", "true"},
		{"--cpus", "100000", "alpine", "true"},
		{"--cpus", "-1", "alpine", "true"},
		{"--pids", "-1", "alpine", "true"},
	} {
		if out, code := microbox(t, args...); code != 2 {
			t.Errorf("microbox run %s: exit %d, want 2\n%s", strings.Join(args, " "), code, out)
		}
	}
}

func TestCgroupRemovedAfterExit(t *testing.T) {
	before := cgroupDirs(t)
	mustRun(t, "--memory", "64m", "alpine", "true")
	for name := range cgroupDirs(t) {
		if !before[name] {
			t.Errorf("leftover cgroup %s", name)
		}
	}
}

func cgroupDirs(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(cgroups.Parent)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	dirs := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			dirs[e.Name()] = true
		}
	}
	return dirs
}
