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
	"strings"
	"syscall"

	"github.com/anishmehta24/microbox/internal/spec"
)

// Command is the hidden argument microbox re-executes itself with.
const Command = "init"

// Main sets up the container and replaces this process with the user's
// command. It only returns control by exiting, after printing why setup failed.
func Main() {
	err := run()
	code := spec.ExitRuntimeError
	var ce *commandError
	if errors.As(err, &ce) {
		code = ce.code
	}
	fmt.Fprintf(os.Stderr, "microbox: init: %v\n", err)
	os.Exit(code)
}

// run only returns on failure: on success the final exec never comes back.
func run() error {
	cfg, err := readConfig()
	if err != nil {
		return err
	}
	if err := syscall.Sethostname([]byte(cfg.Hostname)); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	if err := setupRootfs(cfg.Rootfs); err != nil {
		return err
	}
	return execCommand(cfg.Args, cfg.Env)
}

// readConfig blocks until the runtime has written the config and closed its
// end of the pipe.
func readConfig() (*spec.Config, error) {
	f := os.NewFile(spec.InitPipeFD, "init-pipe")
	defer f.Close() // the user's command must not inherit it
	var cfg spec.Config
	if err := json.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return &cfg, nil
}

// setupRootfs makes rootfs the container's / and mounts a fresh /proc.
func setupRootfs(rootfs string) error {
	// CLONE_NEWNS copied the host's mount table, propagation flags included. On a
	// systemd host / is "shared", so mounts made here would show up on the host
	// too. Make the whole tree private first.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}
	// pivot_root requires new_root to be a mount point. Bind-mounting the
	// directory onto itself makes it one.
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs: %w", err)
	}
	if err := os.Chdir(rootfs); err != nil {
		return err
	}
	// pivot_root(".", ".") is the trick from pivot_root(2): rootfs becomes / and
	// the old root is stacked on top of it at the same place, so no temporary
	// directory is needed inside the image. chroot would only change this
	// process's view of /, and is escapable; pivot_root swaps the root of the
	// whole mount namespace.
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// Detach the old root, with every host mount under it. MNT_DETACH because a
	// plain unmount would fail with EBUSY on a tree full of submounts.
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	// A proc mounted from inside the new PID namespace only lists its processes.
	if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	return nil
}

// execCommand replaces init with the user's command, looked up in the
// container's PATH rather than the host's.
func execCommand(args, env []string) error {
	for _, kv := range env {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			os.Setenv("PATH", p)
		}
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		code := spec.ExitNotFound
		if errors.Is(err, fs.ErrPermission) {
			code = spec.ExitCannotInvoke
		}
		return &commandError{code, err}
	}
	err = syscall.Exec(path, args, env)
	return &commandError{spec.ExitCannotInvoke, fmt.Errorf("exec %s: %w", path, err)}
}

// commandError is a failure to start the user's command, which exits with 126
// or 127 instead of 125.
type commandError struct {
	code int
	err  error
}

func (e *commandError) Error() string { return e.err.Error() }
func (e *commandError) Unwrap() error { return e.err }
