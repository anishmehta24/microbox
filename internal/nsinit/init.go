// Package nsinit is the container side of microbox. It runs as PID 1 inside
// the new namespaces, finishes the setup from the inside and then execs the
// user's command, which takes over PID 1.
package nsinit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/anishmehta24/microbox/internal/spec"
)

// Hidden arguments microbox re-executes itself with.
const (
	Command     = "init"      // PID 1 of a new container
	ExecCommand = "exec-init" // a process that microbox exec started in a running container
)

// Main sets up the container and replaces this process with the user's
// command. It only returns control by exiting, after printing why setup failed.
func Main() {
	exit("init", run())
}

// ExecMain replaces this process with the command microbox exec sent. It
// starts in the container's cgroup and PID namespace, joins the container's
// other namespaces (see container.execIn for why it does that itself), and
// execs the command.
func ExecMain() {
	exit("exec", execMain())
}

func execMain() error {
	// setns changes only the calling thread, so the joining and the final exec
	// must happen on the same one.
	runtime.LockOSThread()
	// Joining a mount namespace changes the thread's root and working
	// directory, which Go's threads share (CLONE_FS), and the kernel refuses
	// to change them under the other threads. Unsharing them first makes it
	// work without the C constructor runc's nsexec uses for this.
	if err := unix.Unshare(unix.CLONE_FS); err != nil {
		return fmt.Errorf("unshare CLONE_FS: %w", err)
	}
	for i, ns := range spec.ExecNamespaces {
		f := os.NewFile(uintptr(spec.ExecNamespaceFD+i), ns)
		err := unix.Setns(int(f.Fd()), 0)
		f.Close() // the command must not inherit it
		if err != nil {
			return fmt.Errorf("join %s namespace: %w", ns, err)
		}
	}
	var p spec.Process
	if err := readConfig(&p); err != nil {
		return err
	}
	return execCommand(p)
}

func exit(stage string, err error) {
	code := spec.ExitRuntimeError
	var ce *commandError
	if errors.As(err, &ce) {
		code = ce.code
	}
	fmt.Fprintf(os.Stderr, "microbox: %s: %v\n", stage, err)
	os.Exit(code)
}

// run only returns on failure: on success the final exec never comes back.
func run() error {
	var cfg spec.Config
	if err := readConfig(&cfg); err != nil {
		return err
	}
	if err := syscall.Sethostname([]byte(cfg.Hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	if err := setupRootfs(&cfg); err != nil {
		return err
	}
	if cfg.Init {
		return reap(cfg.Process)
	}
	return execCommand(cfg.Process)
}

// readConfig blocks until the runtime has written the config and closed its
// end of the pipe.
func readConfig(v any) error {
	f := os.NewFile(spec.InitPipeFD, "init-pipe")
	defer f.Close() // the user's command must not inherit it
	if err := json.NewDecoder(f).Decode(v); err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	return nil
}

// execCommand replaces this process with the user's command.
func execCommand(p spec.Process) error {
	path, err := lookPath(p)
	if err != nil {
		return err
	}
	err = syscall.Exec(path, p.Args, p.Env)
	return &commandError{spec.ExitCannotInvoke, fmt.Errorf("exec %s: %w", path, err)}
}

// lookPath finds the command in the container's PATH rather than the host's.
func lookPath(p spec.Process) (string, error) {
	for _, kv := range p.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			os.Setenv("PATH", v)
		}
	}
	path, err := exec.LookPath(p.Args[0])
	if err != nil {
		code := spec.ExitNotFound
		if errors.Is(err, fs.ErrPermission) {
			code = spec.ExitCannotInvoke
		}
		return "", &commandError{code, err}
	}
	return path, nil
}

// commandError is a failure to start the user's command, which exits with 126
// or 127 instead of 125.
type commandError struct {
	code int
	err  error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }
