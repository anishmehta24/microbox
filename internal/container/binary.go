package container

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/anishmehta24/microbox/internal/spec"
)

// sealedCopy copies microbox's own binary into a sealed memfd, an anonymous
// in-memory file that nothing can change, and returns it read-only.
// Processes inside a container (init, exec-init and --init's reaper) are
// started from this copy instead of the host's binary.
//
// This is the fix runc uses for CVE-2019-5736. A process's /proc/<pid>/exe
// leads to the file it is running. If a process in the container could open
// that link for a microbox process in the container, it would hold the
// host's binary, and once nothing was executing it any more, it could
// overwrite it: the host would run the attacker's code the next time it
// started microbox. With the copy, the link leads to the memfd, which the
// seals keep from being written.
func sealedCopy() (*os.File, error) {
	fd, err := unix.MemfdCreate("microbox", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("memfd_create: %w", err)
	}
	mem := os.NewFile(uintptr(fd), "memfd:microbox")
	defer mem.Close()
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		return nil, err
	}
	defer self.Close()
	if _, err := io.Copy(mem, self); err != nil {
		return nil, fmt.Errorf("copy binary: %w", err)
	}
	seals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(mem.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		return nil, fmt.Errorf("seal memfd: %w", err)
	}
	// execve refuses a file that is open for writing (ETXTBSY), so the
	// caller gets a read-only descriptor, and the writable one is closed.
	return os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
}

// exePath is how a child started with the sealed copy in ExtraFiles, at
// spec.ExeFD, execs it.
var exePath = fmt.Sprintf("/proc/self/fd/%d", spec.ExeFD)

// checkStatic fails if microbox is dynamically linked, which is what a Go
// build with cgo produces. --init runs microbox's binary inside the
// container after pivot_root, and there the kernel would look for the host's
// dynamic loader in the image. Build with CGO_ENABLED=0, as make build does.
func checkStatic() error {
	f, err := elf.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("--init needs a static build of microbox, because the binary runs inside the container where the host's C library isn't available: build with CGO_ENABLED=0 (make build does)")
		}
	}
	return nil
}
