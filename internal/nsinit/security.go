package nsinit

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// defaultCaps is Docker's default capability set: 14 of the 41 capabilities
// root has. It leaves out, among others, CAP_SYS_ADMIN (mount, most
// namespace operations), CAP_SYS_MODULE, CAP_SYS_BOOT, CAP_SYS_PTRACE,
// CAP_SYS_TIME, CAP_NET_ADMIN and CAP_DAC_READ_SEARCH.
var defaultCaps = []uintptr{
	unix.CAP_CHOWN,
	unix.CAP_DAC_OVERRIDE,
	unix.CAP_FSETID,
	unix.CAP_FOWNER,
	unix.CAP_MKNOD,
	unix.CAP_NET_RAW, // ping
	unix.CAP_SETGID,
	unix.CAP_SETUID,
	unix.CAP_SETFCAP,
	unix.CAP_SETPCAP,
	unix.CAP_NET_BIND_SERVICE, // ports below 1024
	unix.CAP_SYS_CHROOT,
	unix.CAP_KILL,
	unix.CAP_AUDIT_WRITE,
}

// restrict limits the calling thread to what the container's processes may
// do, the way Docker's defaults do. Capabilities, no_new_privs and seccomp
// filters all belong to a thread, and are passed on to the processes and
// threads it starts and to the program it execs. So the caller must keep its
// goroutine locked to the thread from here until the exec.
func restrict() error {
	if err := dropCapabilities(); err != nil {
		return err
	}
	// From here on execve can't grant more privileges than the process has,
	// for instance through a setuid binary in the image. It also lets the
	// process install a seccomp filter without CAP_SYS_ADMIN.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	return installSeccomp()
}

// dropCapabilities removes everything but the default set from the bounding
// set and empties the inheritable and ambient sets. The process keeps its
// capabilities until it execs. execve then gives a root process the
// bounding set plus the inheritable set (for root, file capabilities count
// as all set), which leaves exactly the default set. A process running as
// another user gets none, as with Docker.
func dropCapabilities() error {
	last, err := lastCap()
	if err != nil {
		return err
	}
	for c := uintptr(0); c <= last; c++ {
		if slices.Contains(defaultCaps, c) {
			continue
		}
		if err := unix.Prctl(unix.PR_CAPBSET_DROP, c, 0, 0, 0); err != nil {
			return fmt.Errorf("drop capability %d from the bounding set: %w", c, err)
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	// The inheritable set would otherwise survive the exec past the bounding
	// set.
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData // capabilities 0-31 and 32-63
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capget: %w", err)
	}
	data[0].Inheritable, data[1].Inheritable = 0, 0
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capset: %w", err)
	}
	return nil
}

// lastCap is the highest capability this kernel knows.
func lastCap() (uintptr, error) {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 8)
	return uintptr(n), err
}
