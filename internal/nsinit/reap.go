package nsinit

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/anishmehta24/microbox/internal/spec"
)

// reap is --init: it starts the command as a child and stays PID 1 itself,
// doing the two jobs a PID 1 has that most programs don't do:
//
//   - Signals. The kernel drops signals to a namespace's PID 1 that it has no
//     handler for, so `sleep` as PID 1 can't be stopped with SIGTERM or
//     Ctrl-C. reap handles every signal and passes it on to the command,
//     which is an ordinary process with default signal behavior.
//   - Zombies. A process whose parent exits is reparented to PID 1, and its
//     exit status sits in the process table until PID 1 collects it.
//
// reap exits with the command's status. The kernel then kills whatever is
// left in the namespace, as it does whenever a PID 1 exits.
func reap(p spec.Process) error {
	path, err := lookPath(p)
	if err != nil {
		return err
	}
	// Before Start, so no SIGCHLD can be missed.
	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs)

	cmd := exec.Command(path)
	cmd.Args, cmd.Env = p.Args, p.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return &commandError{spec.ExitCannotInvoke, err}
	}
	child := cmd.Process.Pid

	for s := range sigs {
		switch s {
		case syscall.SIGCHLD:
			// One SIGCHLD can stand for several exits, so collect until none
			// are left. cmd.Wait would only collect the command itself.
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				if pid == child {
					os.Exit(exitStatus(ws))
				}
			}
		case syscall.SIGURG:
			// Sent by the Go runtime to itself, to preempt goroutines.
		default:
			syscall.Kill(child, s.(syscall.Signal))
		}
	}
	return nil
}

// exitStatus follows the shell convention, like the runtime does for PID 1:
// killed by signal N becomes 128+N.
func exitStatus(ws syscall.WaitStatus) int {
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
