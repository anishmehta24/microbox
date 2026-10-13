package nsinit

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// deniedSyscalls fail with EPERM inside a container. Most of them already
// fail without the capabilities dropCapabilities removes; the filter blocks
// them even so, and also covers some that need no capability at all: the
// kernel keyring, io_uring, and user namespaces (see seccompFilter).
// Docker's default profile leaves out the same calls.
var deniedSyscalls = []uint32{
	// mounting, including the new mount API
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_FSOPEN, unix.SYS_FSCONFIG,
	unix.SYS_FSMOUNT, unix.SYS_FSPICK, unix.SYS_MOVE_MOUNT, unix.SYS_OPEN_TREE, unix.SYS_MOUNT_SETATTR,
	// new namespaces, and other processes' namespaces
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	// kernel modules
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	// rebooting, or booting another kernel
	unix.SYS_REBOOT, unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
	// the system clock, which is not namespaced
	unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME, unix.SYS_CLOCK_ADJTIME,
	// swap, process accounting, disk quotas
	unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_ACCT, unix.SYS_QUOTACTL, unix.SYS_QUOTACTL_FD,
	// the kernel keyring, which is not namespaced
	unix.SYS_ADD_KEY, unix.SYS_KEYCTL, unix.SYS_REQUEST_KEY,
	// large kernel interfaces that containers rarely need, and that keep
	// turning up in privilege escalation bugs
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	// opening a file by its handle skips the path, and with it the container's
	// root: the "shocker" escape from early Docker
	unix.SYS_OPEN_BY_HANDLE_AT,
	// I/O port access and the kernel log
	unix.SYS_IOPL, unix.SYS_IOPERM, unix.SYS_SYSLOG,
	// obsolete
	unix.SYS_USELIB, unix.SYS_USTAT, unix.SYS_SYSFS, unix.SYS__SYSCTL, unix.SYS_NFSSERVCTL,
	unix.SYS_LOOKUP_DCOOKIE, unix.SYS_CREATE_MODULE, unix.SYS_GET_KERNEL_SYMS, unix.SYS_QUERY_MODULE,
}

// cloneNamespaceFlags are the clone flags that create namespaces. clone(2)
// with any of them fails. Without CAP_SYS_ADMIN only CLONE_NEWUSER would
// still work, and a user namespace makes an unprivileged process root inside
// it, which opens up kernel code that is otherwise reachable only by root.
const cloneNamespaceFlags = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS |
	unix.CLONE_NEWIPC | unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// Return values of a seccomp filter, from linux/seccomp.h.
const (
	retKillProcess = 0x80000000
	retErrno       = 0x00050000 // the low 16 bits are the errno
	retAllow       = 0x7fff0000
)

// Offsets into struct seccomp_data, which the filter reads.
const (
	offNr   = 0  // int nr: the system call number
	offArch = 4  // __u32 arch: AUDIT_ARCH_*
	offArg0 = 16 // __u64 args[0]; its low half, on a little-endian machine
)

// seccompFilter builds the BPF program the kernel runs on each system call.
// It works on struct seccomp_data with one register, A. Jumps are relative
// and only go forward, so the program checks and then jumps to one of four
// return instructions at the end:
//
//	A = arch; not x86-64?                    kill
//	A = nr;   x32 call (bit 30 set)?         kill
//	          in deniedSyscalls?              EPERM
//	          clone3?                         ENOSYS
//	          not clone?                      allow
//	A = clone's flags; a namespace flag?     EPERM
//	                                          allow
//
// Calls from 32-bit code come with another arch, and x32 calls with bit 30
// set; neither has this table's numbers, so both kill the process. clone3
// passes its flags in memory, which BPF can't read, so it fails with
// ENOSYS, and the C library falls back to clone. Docker does the same.
func seccompFilter() []unix.SockFilter {
	n := len(deniedSyscalls)
	allow, eperm, enosys, kill := 8+n, 9+n, 10+n, 11+n

	var p []unix.SockFilter
	stmt := func(code uint16, k uint32) {
		p = append(p, unix.SockFilter{Code: code, K: k})
	}
	// jump appends a conditional jump to instruction jt if true, jf if false.
	// In BPF they are offsets: 0 is the next instruction.
	jump := func(op uint16, k uint32, jt, jf int) {
		i := len(p)
		p = append(p, unix.SockFilter{Code: unix.BPF_JMP | op | unix.BPF_K, K: k, Jt: uint8(jt - i - 1), Jf: uint8(jf - i - 1)})
	}
	load := func(off uint32) { stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, off) }

	load(offArch)
	jump(unix.BPF_JEQ, unix.AUDIT_ARCH_X86_64, 2, kill)
	load(offNr)
	jump(unix.BPF_JGE, 0x40000000, kill, 4)
	for i, nr := range deniedSyscalls {
		jump(unix.BPF_JEQ, nr, eperm, 4+i+1)
	}
	jump(unix.BPF_JEQ, unix.SYS_CLONE3, enosys, 5+n)
	jump(unix.BPF_JEQ, unix.SYS_CLONE, 6+n, allow)
	load(offArg0)
	jump(unix.BPF_JSET, cloneNamespaceFlags, eperm, allow)
	stmt(unix.BPF_RET|unix.BPF_K, retAllow)
	stmt(unix.BPF_RET|unix.BPF_K, retErrno|uint32(unix.EPERM))
	stmt(unix.BPF_RET|unix.BPF_K, retErrno|uint32(unix.ENOSYS))
	stmt(unix.BPF_RET|unix.BPF_K, retKillProcess)
	return p
}

// installSeccomp loads the filter into the calling thread. It needs
// no_new_privs, or CAP_SYS_ADMIN.
func installSeccomp() error {
	prog := seccompFilter()
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&fprog)), 0, 0); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	return nil
}
