package nsinit

import (
	"encoding/binary"
	"testing"

	"golang.org/x/sys/unix"
)

// runFilter runs a seccomp program on one system call, the way the kernel
// does, for the few instructions seccompFilter uses.
func runFilter(t *testing.T, prog []unix.SockFilter, arch, nr uint32, arg0 uint64) uint32 {
	t.Helper()
	data := make([]byte, 64) // struct seccomp_data
	binary.LittleEndian.PutUint32(data[offNr:], nr)
	binary.LittleEndian.PutUint32(data[offArch:], arch)
	binary.LittleEndian.PutUint64(data[offArg0:], arg0)
	var a uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		var cond bool
		switch in.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			a = binary.LittleEndian.Uint32(data[in.K:])
			continue
		case unix.BPF_RET | unix.BPF_K:
			return in.K
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			cond = a == in.K
		case unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K:
			cond = a >= in.K
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			cond = a&in.K != 0
		default:
			t.Fatalf("instruction %d: unexpected code %#x", pc, in.Code)
		}
		if cond {
			pc += int(in.Jt)
		} else {
			pc += int(in.Jf)
		}
	}
	t.Fatal("ran off the end of the program")
	return 0
}

func TestSeccompFilter(t *testing.T) {
	prog := seccompFilter()
	if len(prog) > 255 {
		t.Fatalf("%d instructions: jumps over more than 255 don't fit in a BPF offset", len(prog))
	}
	const x86 = unix.AUDIT_ARCH_X86_64
	eperm, enosys := uint32(retErrno|unix.EPERM), uint32(retErrno|unix.ENOSYS)
	for _, tc := range []struct {
		name     string
		arch, nr uint32
		arg0     uint64
		want     uint32
	}{
		{"read", x86, unix.SYS_READ, 0, retAllow},
		{"execve", x86, unix.SYS_EXECVE, 0, retAllow},
		{"mount", x86, unix.SYS_MOUNT, 0, eperm},
		{"reboot", x86, unix.SYS_REBOOT, 0, eperm},
		{"finit_module", x86, unix.SYS_FINIT_MODULE, 0, eperm},
		{"unshare", x86, unix.SYS_UNSHARE, unix.CLONE_NEWUSER, eperm},
		{"the last denied call", x86, deniedSyscalls[len(deniedSyscalls)-1], 0, eperm},
		{"fork through clone", x86, unix.SYS_CLONE, uint64(unix.SIGCHLD), retAllow},
		{"a thread through clone", x86, unix.SYS_CLONE, unix.CLONE_VM | unix.CLONE_THREAD | unix.CLONE_SIGHAND, retAllow},
		{"clone into a user namespace", x86, unix.SYS_CLONE, unix.CLONE_NEWUSER | uint64(unix.SIGCHLD), eperm},
		{"clone into a network namespace", x86, unix.SYS_CLONE, unix.CLONE_NEWNET, eperm},
		{"clone3", x86, unix.SYS_CLONE3, 0, enosys},
		{"x32 read", x86, 0x40000000 | unix.SYS_READ, 0, retKillProcess},
		{"i386 call", unix.AUDIT_ARCH_I386, 1, 0, retKillProcess},
	} {
		if got := runFilter(t, prog, tc.arch, tc.nr, tc.arg0); got != tc.want {
			t.Errorf("%s: %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

// Docker's default capabilities show up in /proc/<pid>/status as
// CapEff: 00000000a80425fb.
func TestDefaultCapsMatchDocker(t *testing.T) {
	var mask uint64
	for _, c := range defaultCaps {
		mask |= 1 << c
	}
	if mask != 0xa80425fb {
		t.Errorf("mask %#x, want 0xa80425fb", mask)
	}
}
