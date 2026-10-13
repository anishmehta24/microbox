package nsinit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"github.com/anishmehta24/microbox/internal/spec"
)

// setupRootfs builds the container's filesystem and makes it the root of the
// mount namespace.
func setupRootfs(cfg *spec.Config) error {
	// CLONE_NEWNS copied the host's mount table, propagation flags included. On a
	// systemd host / is "shared", so mounts made here would show up on the host
	// too. Make the whole tree private first.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	if err := mountOverlay(cfg.Rootfs); err != nil {
		return err
	}
	if err := pivotRoot(cfg.Rootfs.Merged); err != nil {
		return err
	}
	if err := mountSpecialFilesystems(); err != nil {
		return err
	}
	if err := createDevices(); err != nil {
		return err
	}
	if err := maskPaths(); err != nil {
		return err
	}
	return writeEtcFiles(cfg)
}

// mountOverlay stacks the container's writable layer on the read-only image.
// Reads fall through to the image; the first write to a file copies it up into
// upper, so the image never changes and every container has its own layer.
func mountOverlay(r spec.Rootfs) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", r.Image, r.Upper, r.Work)
	if err := syscall.Mount("overlay", r.Merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	return nil
}

// pivotRoot makes dir the root of the mount namespace. dir must be a mount
// point, which the overlay is.
func pivotRoot(dir string) error {
	if err := os.Chdir(dir); err != nil {
		return err
	}
	// pivot_root(".", ".") is the trick from pivot_root(2): dir becomes / and the
	// old root is stacked on top of it at the same place, so no temporary
	// directory is needed. chroot would only change this process's view of /,
	// and is escapable; pivot_root swaps the root of the whole mount namespace.
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// Detach the old root, with every host mount under it. MNT_DETACH because a
	// plain unmount would fail with EBUSY on a tree full of submounts.
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Chdir("/")
}

type mount struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

// The same set and options as runc's default config. They are mounted after
// pivot_root, so a symlink in the image (say /proc -> /etc) can only point
// somewhere inside the container.
var specialMounts = []mount{
	// Lists only this PID namespace's processes.
	{"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	// Read-only, so the container can look at devices and kernel settings but not change them.
	{"sysfs", "/sys", "sysfs", syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	// Mounted from inside the cgroup namespace, so its root is the container's
	// own group: the container can read its limits and usage but not raise them.
	{"cgroup2", "/sys/fs/cgroup", "cgroup2", syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
	// An empty /dev that only gets the devices below, not the host's disks.
	{"tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID | syscall.MS_STRICTATIME, "mode=755,size=65536k"},
	// newinstance: the container's own pty numbering, without the host's terminals.
	{"devpts", "/dev/pts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5"},
	// POSIX shared memory, capped so a container can't fill RAM through it.
	{"tmpfs", "/dev/shm", "tmpfs", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, "mode=1777,size=65536k"},
	// POSIX message queues of the container's IPC namespace.
	{"mqueue", "/dev/mqueue", "mqueue", syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC, ""},
}

func mountSpecialFilesystems() error {
	for _, m := range specialMounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil {
			return fmt.Errorf("mount %s: %w", m.target, err)
		}
	}
	return nil
}

// The device nodes every container gets, runc's default list.
var devices = []struct {
	name         string
	major, minor uint32
}{
	{"null", 1, 3},
	{"zero", 1, 5},
	{"full", 1, 7},
	{"random", 1, 8},
	{"urandom", 1, 9},
	{"tty", 5, 0}, // the process's controlling terminal, whatever it is
}

var devSymlinks = []struct{ link, target string }{
	{"/dev/fd", "/proc/self/fd"},
	{"/dev/stdin", "/proc/self/fd/0"},
	{"/dev/stdout", "/proc/self/fd/1"},
	{"/dev/stderr", "/proc/self/fd/2"},
	{"/dev/ptmx", "pts/ptmx"}, // the multiplexer of the devpts instance above
}

func createDevices() error {
	old := syscall.Umask(0) // mknod's mode is masked by the umask; these must be exactly 0666
	defer syscall.Umask(old)
	for _, d := range devices {
		path := "/dev/" + d.name
		if err := syscall.Mknod(path, syscall.S_IFCHR|0o666, mkdev(d.major, d.minor)); err != nil {
			return fmt.Errorf("mknod %s: %w", path, err)
		}
	}
	for _, s := range devSymlinks {
		if err := os.Symlink(s.target, s.link); err != nil {
			return err
		}
	}
	return nil
}

// Files under /proc and /sys that show or change the host's kernel rather than
// the container's, the same lists as Docker's. procfs isn't fully namespaced:
// /proc/kcore is the host's memory, and most of /proc/sys and all of
// /proc/sysrq-trigger act on the whole host. Paths this kernel doesn't have
// are skipped.
var (
	// Hidden: /dev/null is bound over files, an empty read-only tmpfs mounted
	// over directories.
	MaskedPaths = []string{"/proc/acpi", "/proc/asound", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
		"/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap"}
	// Readable but not writable.
	ReadonlyPaths = []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
)

func maskPaths() error {
	for _, p := range MaskedPaths {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.IsDir() {
			err = syscall.Mount("tmpfs", p, "tmpfs", syscall.MS_RDONLY, "")
		} else {
			err = syscall.Mount("/dev/null", p, "", syscall.MS_BIND, "")
		}
		if err != nil {
			return fmt.Errorf("mask %s: %w", p, err)
		}
	}
	for _, p := range ReadonlyPaths {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		// Bind the path onto itself, then remount that bind mount read-only.
		// The remount sets the mount's flags rather than adding to them, so
		// it repeats proc's nosuid, nodev and noexec.
		if err := syscall.Mount(p, p, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s: %w", p, err)
		}
		flags := uintptr(syscall.MS_BIND | syscall.MS_REMOUNT | syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV | syscall.MS_NOEXEC)
		if err := syscall.Mount(p, p, "", flags, ""); err != nil {
			return fmt.Errorf("make %s read-only: %w", p, err)
		}
	}
	return nil
}

// mkdev packs a device number like glibc's makedev (sys/sysmacros.h): the
// minor's low 8 bits, then 12 bits of major, then the rest of the minor, then
// the rest of the major.
func mkdev(major, minor uint32) int {
	return int(uint64(minor&0xff) | uint64(major&0xfff)<<8 | uint64(minor&^0xff)<<12 | uint64(major&^0xfff)<<32)
}

// writeEtcFiles writes /etc/hostname, /etc/hosts and /etc/resolv.conf. They
// are written after pivot_root, so even a symlinked /etc in the image resolves
// inside the container and can't make microbox write to a host file. Existing
// files are removed first, so a symlink gets replaced instead of followed. The
// writes go through the overlay into the container's upper layer.
func writeEtcFiles(cfg *spec.Config) error {
	files := map[string]string{
		"/etc/hostname": cfg.Hostname + "\n",
		"/etc/hosts":    cfg.Hosts,
	}
	if cfg.ResolvConf != "" {
		files["/etc/resolv.conf"] = cfg.ResolvConf
	}
	if err := os.MkdirAll("/etc", 0o755); err != nil {
		return err
	}
	for path, data := range files {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			return err
		}
	}
	return nil
}
