// Package container is the host side of microbox: it prepares a container,
// starts init inside new namespaces and waits for it.
package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/anishmehta24/microbox/internal/nsinit"
	"github.com/anishmehta24/microbox/internal/spec"
)

// ImagesDir holds unpacked root filesystems, one directory per image.
const ImagesDir = "/var/lib/microbox/images"

// Namespaces every container gets. Cgroup (M3) and network (M4) namespaces are
// added by later milestones.
const cloneFlags = syscall.CLONE_NEWUTS | // own hostname
	syscall.CLONE_NEWPID | // own process tree; the first process is PID 1
	syscall.CLONE_NEWNS | // own mount table, so pivot_root does not touch the host
	syscall.CLONE_NEWIPC // own System V IPC and POSIX message queues

// Run implements `microbox run` and returns the exit code for microbox.
func Run(args []string) int {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	hostname := fl.String("hostname", "", "container hostname (default: the container ID)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "usage: microbox run [--hostname NAME] IMAGE COMMAND [ARG...]")
		return 2
	}
	code, err := run(*hostname, fl.Arg(0), fl.Args()[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
		return spec.ExitRuntimeError
	}
	return code
}

func run(hostname, image string, argv []string) (int, error) {
	if os.Geteuid() != 0 {
		return 0, errors.New("must run as root: new namespaces, mounts and pivot_root need CAP_SYS_ADMIN")
	}
	rootfs, err := resolveImage(image)
	if err != nil {
		return 0, err
	}
	id, err := newID()
	if err != nil {
		return 0, err
	}
	if hostname == "" {
		hostname = id
	}
	cfg := spec.Config{ID: id, Hostname: hostname, Rootfs: rootfs, Args: argv, Env: defaultEnv(hostname)}

	// init reads its config from this pipe and does nothing until it arrives, so
	// the write is also the "go" signal. Later milestones put the child into its
	// cgroup and network namespace between Start and that write.
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer pw.Close()

	// The Go runtime is multithreaded, and a multithreaded process cannot move
	// itself into a new mount namespace. So instead of unsharing in place,
	// microbox starts a fresh copy of itself with clone flags.
	cmd := exec.Command("/proc/self/exe", nsinit.Command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{pr}
	cmd.Env = []string{} // the container env travels in cfg, keep the host's out
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: cloneFlags,
		Pdeathsig:  syscall.SIGKILL, // if microbox dies, the container dies with it
	}

	// Pdeathsig fires when the thread that forked the child exits, not the whole
	// process. Pin this goroutine to its thread so the Go scheduler cannot retire
	// that thread while the container is running.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Registered before Start: signals Go handles are reset to the default in the
	// child, so the container does not inherit this.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		pr.Close()
		return 0, fmt.Errorf("start init: %w", err)
	}
	pr.Close() // the child has its own copy
	go forwardSignals(sigs, cmd.Process)

	if err := json.NewEncoder(pw).Encode(cfg); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("send config to init: %w", err)
	}
	pw.Close()

	err = cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, fmt.Errorf("wait for container: %w", err)
	}
	return exitCode(cmd.ProcessState), nil
}

// forwardSignals relays SIGTERM and SIGHUP to the container. SIGINT and SIGQUIT
// come from the terminal, which already sends them to the whole foreground
// process group, container included. microbox only has to survive them.
//
// A PID 1 without a handler ignores them anyway (same as docker run without
// --init). That is the reason for the --init option planned in M5.
func forwardSignals(sigs <-chan os.Signal, p *os.Process) {
	for s := range sigs {
		if s == syscall.SIGTERM || s == syscall.SIGHUP {
			p.Signal(s)
		}
	}
}

// exitCode follows the shell convention: a process killed by signal N exits 128+N.
func exitCode(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// resolveImage maps a bare name like "alpine" to ImagesDir/alpine; anything with
// a slash is used as a path.
func resolveImage(image string) (string, error) {
	dir := image
	if !strings.Contains(image, "/") {
		dir = filepath.Join(ImagesDir, image)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("image %q not found at %s (try: sudo scripts/get-alpine.sh)", image, dir)
	}
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("image %s is not a directory", dir)
	}
	return dir, nil
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func defaultEnv(hostname string) []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"HOSTNAME=" + hostname,
	}
	if term := os.Getenv("TERM"); term != "" {
		env = append(env, "TERM="+term)
	}
	return env
}
