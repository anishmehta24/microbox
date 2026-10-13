package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/anishmehta24/microbox/internal/cgroups"
	"github.com/anishmehta24/microbox/internal/nsinit"
	"github.com/anishmehta24/microbox/internal/spec"
)

// Exec implements `microbox exec CONTAINER COMMAND [ARG...]`: it runs another
// process in a running container, with the container's namespaces, cgroup and
// environment, and exits with that process's exit code.
func Exec(args []string) int {
	if len(args) < 2 {
		return usage("usage: microbox exec CONTAINER COMMAND [ARG...]")
	}
	code, err := execIn(args[0], args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
		return spec.ExitRuntimeError
	}
	return code
}

// execIn starts exec-init inside the container, which joins the container's
// namespaces and execs the command. The work is split between the two:
//
//   - The PID namespace has to be joined here. setns(CLONE_NEWPID) never
//     moves the caller, only the children it forks afterwards.
//   - exec-init joins the other namespaces itself, after it has started. If
//     this process joined the mount namespace before forking, the kernel
//     would load microbox's binary with the container's / and look for the
//     dynamic loader (/lib64/ld-linux-x86-64.so.2) in the image. And it is
//     cloned straight into the container's cgroup, which the kernel only
//     allows from outside the container's cgroup namespace: cgroup2 is
//     mounted with nsdelegate, and then a process in a cgroup namespace may
//     only move processes between groups inside it.
func execIn(ref string, argv []string) (int, error) {
	if os.Geteuid() != 0 {
		return 0, errors.New("must run as root: joining a container's namespaces needs CAP_SYS_ADMIN")
	}
	c, err := find(ref)
	if err != nil {
		return 0, err
	}
	if c.State() != Running {
		return 0, fmt.Errorf("container %s is not running", c.ID)
	}
	var nsFiles []*os.File
	defer func() {
		for _, f := range nsFiles {
			f.Close()
		}
	}()
	for _, ns := range append([]string{"pid"}, spec.ExecNamespaces...) {
		f, err := os.Open("/proc/" + strconv.Itoa(c.Pid1.Pid) + "/ns/" + ns)
		if err != nil {
			return 0, fmt.Errorf("container %s is not running: %w", c.ID, err)
		}
		nsFiles = append(nsFiles, f)
	}
	// Checked after opening: if the PID now belongs to some other process,
	// these are that process's namespaces.
	if !c.Pid1.alive() {
		return 0, fmt.Errorf("container %s is not running", c.ID)
	}
	cgDir, err := os.Open(cgroups.Lookup(c.ID).Path)
	if err != nil {
		return 0, err
	}
	defer cgDir.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer pw.Close()

	// setns changes only the calling thread, and a child gets its namespaces
	// from the thread that forks it. So this goroutine stays on one thread,
	// which joins and then forks. It is never unlocked, so no other goroutine
	// runs on a thread that is half inside the container.
	runtime.LockOSThread()
	if err := unix.Setns(int(nsFiles[0].Fd()), unix.CLONE_NEWPID); err != nil {
		return 0, fmt.Errorf("join pid namespace: %w", err)
	}

	cmd := exec.Command("/proc/self/exe", nsinit.ExecCommand)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = append([]*os.File{pr}, nsFiles[1:]...) // fd 3, then spec.ExecNamespaces from fd 4
	cmd.Env = []string{}
	// No Pdeathsig: Go's fork code checks that the child's parent is still
	// alive by comparing getppid() with microbox's PID. microbox is outside
	// the container's PID namespace, so getppid() returns 0 there and the
	// child would kill itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(cgDir.Fd())}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	err = cmd.Start()
	pr.Close()
	if err != nil {
		return 0, fmt.Errorf("start process in container: %w", err)
	}
	go forwardSignals(sigs, cmd.Process)

	p := spec.Process{Args: argv, Env: execEnv(c.Config.Process.Env)}
	if err := json.NewEncoder(pw).Encode(p); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("send command: %w", err)
	}
	pw.Close()
	err = cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return 0, err
	}
	return exitCode(cmd.ProcessState), nil
}

// execEnv is the container's environment with the caller's terminal type,
// which may differ from the one the container was started from.
func execEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TERM=") {
			out = append(out, kv)
		}
	}
	if term := os.Getenv("TERM"); term != "" {
		out = append(out, "TERM="+term)
	}
	return out
}
