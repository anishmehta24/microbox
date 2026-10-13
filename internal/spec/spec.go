// Package spec holds what the runtime (parent) and init (child) share: the
// container config and the exit codes.
package spec

// Config is everything init needs to set up the container. The runtime sends it
// as JSON over the init pipe.
type Config struct {
	ID       string  `json:"id"`
	Hostname string  `json:"hostname"`
	Rootfs   Rootfs  `json:"rootfs"`
	Process  Process `json:"process"`

	// Contents of /etc/hosts and /etc/resolv.conf. They depend on the
	// network, which the runtime sets up, so the runtime writes them.
	Hosts      string `json:"hosts"`
	ResolvConf string `json:"resolvConf"`

	// Init keeps microbox's init as PID 1: it starts the command as its child,
	// passes signals on to it and reaps orphaned processes (--init).
	Init bool `json:"init"`
}

// Process is a command to run in the container. microbox exec sends one on its
// own to start an extra process in a running container.
type Process struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

// Rootfs is the container's root filesystem: an overlay of the read-only image
// and a writable layer of its own. All paths are absolute host paths.
type Rootfs struct {
	Image  string `json:"image"`  // lowerdir, never written to
	Upper  string `json:"upper"`  // the container's changes
	Work   string `json:"work"`   // overlayfs scratch space, same filesystem as Upper
	Merged string `json:"merged"` // where the overlay is mounted; becomes /
}

// File descriptors the runtime passes to init and exec-init, as entries of
// exec.Cmd.ExtraFiles: entry i always lands on fd 3+i.
const (
	// ExeFD is the sealed copy of microbox's binary the process was
	// started from. The process closes it first thing.
	ExeFD = 3
	// InitPipeFD is where init reads its Config, and exec-init its Process.
	InitPipeFD = 4

	// StartedFD (init only) is the write end of a pipe the runtime reads
	// until it closes. It closes once the command runs: with the exec, or
	// with --init once the reaper handles signals. Only then does the
	// runtime treat the container as running, because until then a signal
	// reaches microbox's own code instead of the command.
	StartedFD = 5

	// ExecNamespaceFD (exec-init only) is the first of ExecNamespaces.
	ExecNamespaceFD = 5
)

// ExecNamespaces are the namespaces exec-init joins itself, in this order,
// from ExecNamespaceFD on. Mount is last: after it, the process no longer
// sees the host's /proc.
var ExecNamespaces = []string{"ipc", "uts", "net", "cgroup", "mnt"}

// Exit codes for failures before the user's command runs. Same meaning as in
// docker run: 125 is a runtime error, 126 means the command could not be
// executed, 127 means it was not found.
const (
	ExitRuntimeError = 125
	ExitCannotInvoke = 126
	ExitNotFound     = 127
)
