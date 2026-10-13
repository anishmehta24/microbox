package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/anishmehta24/microbox/internal/container"
)

var idRE = regexp.MustCompile(`^[0-9a-f]{12}$`)

// runDetached starts a container with run -d and returns its ID. It is
// removed when the test ends.
func runDetached(t *testing.T, args ...string) string {
	t.Helper()
	id := mustCLI(t, append([]string{"run", "-d"}, args...)...)
	if !idRE.MatchString(id) {
		t.Fatalf("run -d printed %q, want a container ID", id)
	}
	t.Cleanup(func() { exec.Command(bin, "rm", "-f", id).Run() })
	return id
}

// record reads a container's state.json.
func record(t *testing.T, id string) *container.Container {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(container.ContainersDir, id, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c container.Container
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

// psLine returns the container's line in ps -a, or "" if it isn't listed.
func psLine(t *testing.T, id string) string {
	t.Helper()
	for _, line := range strings.Split(mustCLI(t, "ps", "-a"), "\n") {
		if strings.HasPrefix(line, id) {
			return line
		}
	}
	return ""
}

func TestDetachedRun(t *testing.T) {
	id := runDetached(t, "alpine", "sh", "-c", "echo hello from $(hostname); exec sleep 60")
	if line := psLine(t, id); !strings.Contains(line, "Up ") || !strings.Contains(line, `"sh -c echo hello fr…"`) {
		t.Errorf("ps line: %q", line)
	}
	running := strings.Split(mustCLI(t, "ps"), "\n")
	if !slices.ContainsFunc(running, func(l string) bool { return strings.HasPrefix(l, id) }) {
		t.Errorf("ps without -a doesn't list the running container:\n%s", strings.Join(running, "\n"))
	}
	if got := waitForLog(t, id, "hello"); got != "hello from "+id {
		t.Errorf("logs = %q", got)
	}
}

// waitForLog polls microbox logs until the output contains want.
func waitForLog(t *testing.T, id, want string) string {
	t.Helper()
	var out string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if out = mustCLI(t, "logs", id); strings.Contains(out, want) {
			break
		}
	}
	return out
}

// The terminal that ran `run -d` goes away, and with it its session: the
// kernel sends SIGHUP to the processes attached to it. The supervisor is in a
// session of its own, with no terminal.
func TestDetachedSurvivesTerminalClose(t *testing.T) {
	if skipReason != "" {
		t.Skip(skipReason)
	}
	if _, err := exec.LookPath("script"); err != nil {
		t.Skip("needs script(1) to give microbox a terminal")
	}
	out, err := exec.Command("script", "-qec", bin+" run -d alpine sleep 60", "/dev/null").Output()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(out))
	if !idRE.MatchString(id) {
		t.Fatalf("run -d printed %q", out)
	}
	t.Cleanup(func() { exec.Command(bin, "rm", "-f", id).Run() })

	time.Sleep(300 * time.Millisecond) // after script has exited and its terminal is gone
	if line := psLine(t, id); !strings.Contains(line, "Up ") {
		t.Errorf("container didn't survive its terminal: %q", line)
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", record(t, id).Supervisor.Pid))
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	if f[3] != f[2] || f[4] != "0" { // session == own pgid, tty_nr == 0
		t.Errorf("supervisor: session %s, pgid %s, tty %s; want its own session and no terminal", f[3], f[2], f[4])
	}
}

// sleep as PID 1 ignores SIGTERM, so stop kills it after the timeout.
func TestStopKillsAfterTimeout(t *testing.T) {
	id := runDetached(t, "alpine", "sleep", "60")
	start := time.Now()
	mustCLI(t, "stop", "-t", "1", id)
	if d := time.Since(start); d < time.Second || d > 5*time.Second {
		t.Errorf("stop took %v, want about the 1 s timeout", d)
	}
	if line := psLine(t, id); !strings.Contains(line, "Exited (137)") {
		t.Errorf("ps line after stop: %q", line)
	}
}

// With --init, PID 1 passes SIGTERM on to sleep, which exits at once.
func TestInitForwardsSignals(t *testing.T) {
	id := runDetached(t, "--init", "alpine", "sleep", "60")
	start := time.Now()
	mustCLI(t, "stop", id)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stop took %v, want well under the 10 s timeout", d)
	}
	if line := psLine(t, id); !strings.Contains(line, "Exited (143)") {
		t.Errorf("ps line after stop: %q", line)
	}
	ps := mustCLI(t, "exec", runDetached(t, "--init", "alpine", "sleep", "60"), "ps", "-o", "pid,args")
	if !regexp.MustCompile(`(?m)^\s+1 .*microbox reap sleep 60$`).MatchString(ps) || !regexp.MustCompile(`(?m)^\s+\d+ sleep 60$`).MatchString(ps) {
		t.Errorf("with --init, PID 1 should be microbox's reaper and sleep its child:\n%s", ps)
	}
}

// The subshell exits right away, so its sleep is reparented to PID 1. When
// the sleep exits, only PID 1 can collect it. Without --init, PID 1 is the
// second sleep, which never does.
func TestInitReapsZombies(t *testing.T) {
	script := "(sleep 0.1 &); exec sleep 60"
	without := runDetached(t, "alpine", "sh", "-c", script)
	with := runDetached(t, "--init", "alpine", "sh", "-c", script)
	time.Sleep(time.Second)
	if n := zombies(t, without); n != 1 {
		t.Errorf("without --init: %d zombies, want 1", n)
	}
	if n := zombies(t, with); n != 0 {
		t.Errorf("with --init: %d zombies, want 0", n)
	}
}

// zombies counts the zombie children of the container's PID 1. A zombie is no
// longer in its cgroup's cgroup.procs, so this looks through /proc.
func zombies(t *testing.T, id string) int {
	t.Helper()
	pid1 := strconv.Itoa(record(t, id).Pid1.Pid)
	stats, _ := filepath.Glob("/proc/[0-9]*/stat")
	n := 0
	for _, path := range stats {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:])) // state ppid ...
		if f[0] == "Z" && f[1] == pid1 {
			n++
		}
	}
	return n
}

func TestExec(t *testing.T) {
	id := runDetached(t, "--hostname", "box3", "alpine", "sleep", "60")
	if got := mustCLI(t, "exec", id[:6], "hostname"); got != "box3" {
		t.Errorf("hostname = %q, want box3", got)
	}
	// Every namespace is the same as PID 1's, and the cgroup is the container's,
	// which now counts the exec'd process too.
	script := `for n in ipc uts net pid mnt cgroup; do
			[ "$(readlink /proc/self/ns/$n)" = "$(readlink /proc/1/ns/$n)" ] || echo "$n differs"
		done
		cat /proc/self/cgroup
		[ $(cat /sys/fs/cgroup/pids.current) -gt 1 ] && echo counted`
	if got := mustCLI(t, "exec", id, "sh", "-c", script); got != "0::/\ncounted" {
		t.Errorf("got:\n%s\nwant the same namespaces, cgroup 0::/ and more than one process in it", got)
	}
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"sh", "-c", "exit 3"}, 3},
		{[]string{"no-such-command"}, 127},
		{[]string{"/etc/passwd"}, 126},
	} {
		if out, code := cli(t, append([]string{"exec", id}, tc.args...)...); code != tc.want {
			t.Errorf("exec %v: exit %d, want %d\n%s", tc.args, code, tc.want, out)
		}
	}
	mustCLI(t, "stop", "-t", "0", id)
	if out, code := cli(t, "exec", id, "true"); code != 125 || !strings.Contains(out, "not running") {
		t.Errorf("exec into a stopped container: exit %d\n%s", code, out)
	}
}

// After stop and rm nothing of the container is left on the host.
func TestStopAndRmLeaveNothing(t *testing.T) {
	before := hostState(t)
	id := runDetached(t, "-p", "18083:80", "alpine", "sleep", "60")
	if rules := natRules(t); !strings.Contains(rules, "microbox:"+id) {
		t.Errorf("no port rules tagged with the container ID:\n%s", rules)
	}
	mustCLI(t, "stop", "-t", "0", id)
	mustCLI(t, "rm", id)
	if after := hostState(t); after != before {
		t.Errorf("host state changed:\nbefore %s\nafter  %s", before, after)
	}
}

// hostState sums up what containers add on the host.
func hostState(t *testing.T) string {
	t.Helper()
	var rules []string
	for _, line := range strings.Split(natRules(t), "\n") {
		if strings.Contains(line, "microbox:") {
			rules = append(rules, line)
		}
	}
	return fmt.Sprintf("%s; cgroups %v; containers %v; rules %q; mounts %d", networkState(t),
		keys(cgroupDirs(t)), keys(containerDirs(t)), rules, countLines(t, "/proc/self/mountinfo"))
}

func natRules(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("iptables", "-w", "-t", "nat", "-S").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func keys(m map[string]bool) []string {
	var k []string
	for name := range m {
		k = append(k, name)
	}
	slices.Sort(k)
	return k
}

func TestRm(t *testing.T) {
	id := runDetached(t, "alpine", "sleep", "60")
	if out, code := cli(t, "rm", id); code != 1 || !strings.Contains(out, "is running") {
		t.Errorf("rm of a running container: exit %d\n%s", code, out)
	}
	if line := psLine(t, id); !strings.Contains(line, "Up ") {
		t.Errorf("the failed rm stopped the container: %q", line)
	}
	if got := mustCLI(t, "rm", "-f", id); got != id {
		t.Errorf("rm -f printed %q", got)
	}
	if _, err := os.Stat(filepath.Join(container.ContainersDir, id)); !os.IsNotExist(err) {
		t.Errorf("container directory still there: %v", err)
	}
	if out, code := cli(t, "rm", id); code != 1 || !strings.Contains(out, "no such container") {
		t.Errorf("second rm: exit %d\n%s", code, out)
	}
}

// Without --rm a container stays after it exits, as in docker, until rm.
func TestContainerKeptUntilRm(t *testing.T) {
	before := containerDirs(t)
	if _, code := cli(t, "run", "alpine", "sh", "-c", "exit 4"); code != 4 {
		t.Fatalf("exit %d, want 4", code)
	}
	var id string
	for name := range containerDirs(t) {
		if !before[name] {
			id = name
		}
	}
	if id == "" {
		t.Fatal("no container directory left after the run")
	}
	if line := psLine(t, id); !strings.Contains(line, "Exited (4)") {
		t.Errorf("ps line: %q", line)
	}
	if out, code := cli(t, "logs", id); code != 1 || !strings.Contains(out, "foreground") {
		t.Errorf("logs of a foreground container: exit %d\n%s", code, out)
	}
	mustCLI(t, "rm", id)
	if containerDirs(t)[id] {
		t.Error("rm left the directory")
	}
}

func TestLogsFollow(t *testing.T) {
	id := runDetached(t, "alpine", "sh", "-c", "for i in 1 2 3; do echo line $i; sleep 0.2; done")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "logs", "-f", id).Output()
	if err != nil {
		t.Fatalf("logs -f: %v", err)
	}
	if got := string(out); got != "line 1\nline 2\nline 3\n" {
		t.Errorf("logs -f = %q", got)
	}
}

// Killing the supervisor kills the container through Pdeathsig, but nothing
// is left to clean up after it. ps shows it as dead, and gc removes it with
// its cgroup, address and port rules.
func TestGC(t *testing.T) {
	mustCLI(t, "gc") // so that it has only this test's leftovers to remove
	before := hostState(t)
	ids := containerDirs(t)
	run := exec.Command(bin, "run", "-p", "18084:80", "alpine", "sleep", "60")
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	var id string
	for deadline := time.Now().Add(5 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for name := range containerDirs(t) {
			b, err := os.ReadFile(filepath.Join(container.ContainersDir, name, "state.json"))
			if !ids[name] && err == nil && strings.Contains(string(b), `"status": "running"`) {
				id = name
			}
		}
	}
	if id == "" {
		t.Fatal("the container never started running")
	}
	run.Process.Signal(syscall.SIGKILL)
	run.Wait()

	if line := psLine(t, id); !strings.Contains(line, "Dead") {
		t.Errorf("ps line after killing the supervisor: %q", line)
	}
	if hostState(t) == before {
		t.Error("nothing left behind to clean up; the test proves nothing")
	}
	if got := mustCLI(t, "gc"); !strings.Contains(got, "removed dead container "+id) {
		t.Errorf("gc printed:\n%s", got)
	}
	if after := hostState(t); after != before {
		t.Errorf("after gc:\n%s\nwant\n%s", after, before)
	}
}

// A detached container that can't start fails run -d and leaves nothing.
func TestDetachedStartFailure(t *testing.T) {
	before := hostState(t)
	id := runDetached(t, "-p", "18085:80", "alpine", "sleep", "60")
	out, code := cli(t, "run", "-d", "-p", "18085:80", "alpine", "true")
	if code != 125 || !strings.Contains(out, "publish port 18085") {
		t.Errorf("exit %d, want 125 and a port error:\n%s", code, out)
	}
	mustCLI(t, "rm", "-f", id)
	if after := hostState(t); after != before {
		t.Errorf("after the failed start:\n%s\nwant\n%s", after, before)
	}
}
