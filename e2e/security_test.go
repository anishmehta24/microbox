package e2e

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Docker's default capabilities, in /proc/<pid>/status form.
const defaultCaps = "00000000a80425fb"

const statusScript = `grep -E '^(Cap(Inh|Prm|Eff|Bnd|Amb)|NoNewPrivs|Seccomp):' /proc/self/status`

var wantStatus = strings.Join([]string{
	"CapInh:\t0000000000000000",
	"CapPrm:\t" + defaultCaps,
	"CapEff:\t" + defaultCaps,
	"CapBnd:\t" + defaultCaps,
	"CapAmb:\t0000000000000000",
	"NoNewPrivs:\t1",
	"Seccomp:\t2", // a filter
}, "\n")

func TestRestrictedProcess(t *testing.T) {
	if got := mustRun(t, "alpine", "sh", "-c", statusScript); got != wantStatus {
		t.Errorf("got:\n%s\nwant:\n%s", got, wantStatus)
	}
}

// The "done when" of M6. Each of these works for root on the host.
func TestPrivilegedOperationsFail(t *testing.T) {
	for _, tc := range []struct{ name, cmd, want string }{
		{"mount", "mount -t tmpfs none /mnt", "mount: permission denied"},
		{"reboot", "reboot -f", "reboot: : Operation not permitted"},
		// The file isn't a module, but it doesn't get that far: on the host,
		// insmod fails with "Exec format error".
		{"load a kernel module", "insmod /etc/hostname", "insmod: can't insert '/etc/hostname': Operation not permitted"},
		{"create a user namespace", "unshare -U true", "unshare(0x10000000): Operation not permitted"},
		{"create a mount namespace", "unshare -m true", "unshare(0x20000): Operation not permitted"},
		{"set the clock", "date -s 2000-01-01", "date: can't set date: Operation not permitted"},
	} {
		if out := mustRun(t, "alpine", "sh", "-c", tc.cmd+"; echo still running"); !strings.Contains(out, tc.want) || !strings.HasSuffix(out, "still running") {
			t.Errorf("%s: got %q, want it to fail with %q", tc.name, out, tc.want)
		}
	}
}

// The default capabilities still cover what ordinary programs do as root.
func TestDefaultCapabilitiesWork(t *testing.T) {
	script := fmt.Sprintf(`touch /tmp/f && chown 1:1 /tmp/f && mknod /tmp/null c 1 3 &&
		ping -c 1 -W 2 %s >/dev/null && echo ok`, "10.88.0.1")
	if got := mustRun(t, "alpine", "sh", "-c", script); got != "ok" {
		t.Errorf("got %q", got)
	}
}

func TestMaskedPaths(t *testing.T) {
	script := `wc -c < /proc/kcore
		echo x > /proc/sys/kernel/domainname
		echo h > /proc/sysrq-trigger
		ls /sys/firmware | wc -l`
	want := "0\nsh: can't create /proc/sys/kernel/domainname: Read-only file system\n" +
		"sh: can't create /proc/sysrq-trigger: Read-only file system\n0"
	if got := mustRun(t, "alpine", "sh", "-c", script); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestExecIsRestricted(t *testing.T) {
	id := runDetached(t, "alpine", "sleep", "60")
	if got := mustCLI(t, "exec", id, "sh", "-c", statusScript); got != wantStatus {
		t.Errorf("got:\n%s\nwant:\n%s", got, wantStatus)
	}
}

// --init's PID 1 runs as long as the container. Every one of its threads has
// the container's capabilities, it isn't dumpable, so the container can't
// open its /proc/1/exe, and that leads to the sealed copy of microbox
// anyway, not to the binary on the host.
func TestInitIsRestricted(t *testing.T) {
	id := runDetached(t, "--init", "alpine", "sleep", "60")
	script := `grep -h -E '^Cap(Eff|Bnd)' /proc/1/task/*/status | sort -u
		readlink /proc/1/exe || echo "can't read /proc/1/exe"`
	want := "CapBnd:\t" + defaultCaps + "\nCapEff:\t" + defaultCaps + "\ncan't read /proc/1/exe"
	if got, _ := cli(t, "exec", id, "sh", "-c", script); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", record(t, id).Pid1.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if exe != "/memfd:microbox (deleted)" {
		t.Errorf("PID 1 runs %s, want the sealed copy", exe)
	}
}
