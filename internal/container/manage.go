package container

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/anishmehta24/microbox/internal/cgroups"
	"github.com/anishmehta24/microbox/internal/network"
)

// Ps implements `microbox ps [-a]`.
func Ps(args []string) int {
	fl := flag.NewFlagSet("ps", flag.ContinueOnError)
	all := fl.Bool("a", false, "show all containers, not only running ones")
	if err := fl.Parse(args); err != nil || fl.NArg() > 0 {
		return usage("usage: microbox ps [-a]")
	}
	cs, err := list()
	if err != nil {
		return fail(err)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tPORTS")
	now := time.Now()
	for _, c := range cs {
		state := c.State()
		if state != Running && !*all {
			continue
		}
		var ports []string
		for _, p := range c.Ports {
			ports = append(ports, fmt.Sprintf("%d->%d/tcp", p.Host, p.Container))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\t%s\t%s\n", c.ID, c.Image, quoteCommand(c.Config.Process.Args),
			since(c.Created, now), describe(c, state, now), strings.Join(ports, ", "))
	}
	w.Flush()
	return 0
}

func describe(c *Container, state Status, now time.Time) string {
	switch state {
	case Running:
		return "Up " + since(c.Created, now)
	case Exited:
		return fmt.Sprintf("Exited (%d) %s ago", c.ExitCode, since(c.Finished, now))
	case Dead:
		return "Dead (supervisor gone, see microbox gc)"
	}
	return "Created"
}

func since(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// quoteCommand shortens the command to fit a column, like docker ps.
func quoteCommand(args []string) string {
	cmd := strings.Join(args, " ")
	if r := []rune(cmd); len(r) > 20 {
		cmd = string(r[:19]) + "…"
	}
	return `"` + cmd + `"`
}

// Stop implements `microbox stop [-t SECONDS] CONTAINER...`.
func Stop(args []string) int {
	fl := flag.NewFlagSet("stop", flag.ContinueOnError)
	timeout := fl.Int("t", 10, "seconds to wait after SIGTERM before killing")
	if err := fl.Parse(args); err != nil || fl.NArg() == 0 || *timeout < 0 {
		return usage("usage: microbox stop [-t SECONDS] CONTAINER...")
	}
	return forEach(fl.Args(), func(c *Container) error {
		return stop(c, time.Duration(*timeout)*time.Second)
	})
}

// stop asks the container's PID 1 to exit with SIGTERM, like docker stop, and
// kills everything in the container if it hasn't exited after timeout. It
// returns once the supervisor has recorded the exit, so by then the cgroup and
// the network are gone too.
//
// The SIGTERM only works if PID 1 handles it: the kernel drops signals to a
// namespace's PID 1 that it has no handler for. A shell or `sleep` as PID 1
// ignores it and gets killed after the timeout, unless the container runs
// with --init.
func stop(c *Container, timeout time.Duration) error {
	if c.State() != Running {
		return nil
	}
	if timeout > 0 {
		if c.Pid1.alive() {
			syscall.Kill(c.Pid1.Pid, syscall.SIGTERM)
		}
		if waitExit(c.ID, timeout) {
			return nil
		}
	}
	if err := cgroups.Lookup(c.ID).Kill(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !waitExit(c.ID, 10*time.Second) {
		return fmt.Errorf("container %s is still running after SIGKILL", c.ID)
	}
	return nil
}

// waitExit polls the record until the container is no longer running: its
// supervisor recorded the exit, removed the container (--rm), or is gone.
func waitExit(id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c, err := load(id)
		if err != nil || c.State() != Running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Rm implements `microbox rm [-f] CONTAINER...`.
func Rm(args []string) int {
	fl := flag.NewFlagSet("rm", flag.ContinueOnError)
	force := fl.Bool("f", false, "kill the container first if it is running")
	if err := fl.Parse(args); err != nil || fl.NArg() == 0 {
		return usage("usage: microbox rm [-f] CONTAINER...")
	}
	return forEach(fl.Args(), func(c *Container) error {
		switch c.State() {
		case Created:
			return fmt.Errorf("container %s is starting", c.ID)
		case Running:
			if !*force {
				return fmt.Errorf("container %s is running: stop it first, or use rm -f", c.ID)
			}
			if err := stop(c, 0); err != nil {
				return err
			}
		case Dead:
			if err := cleanupDead(c.ID); err != nil {
				return err
			}
		}
		return remove(c.ID)
	})
}

// cleanupDead releases what a dead container's supervisor would have released
// after the exit.
func cleanupDead(id string) error {
	return errors.Join(cgroups.Lookup(id).Destroy(), network.Cleanup(id))
}

// Logs implements `microbox logs [-f] CONTAINER`.
func Logs(args []string) int {
	fl := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fl.Bool("f", false, "keep printing new output until the container exits")
	if err := fl.Parse(args); err != nil || fl.NArg() != 1 {
		return usage("usage: microbox logs [-f] CONTAINER")
	}
	c, err := find(fl.Arg(0))
	if err != nil {
		return fail(err)
	}
	f, err := os.Open(logPath(c.ID))
	if errors.Is(err, fs.ErrNotExist) {
		return fail(fmt.Errorf("container %s ran in the foreground: its output went to the terminal, not to a log", c.ID))
	}
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	for {
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return fail(err)
		}
		if !*follow {
			return 0
		}
		if c, err := load(c.ID); err != nil || c.State() != Running {
			// Whatever it wrote before exiting is in the file by now.
			io.Copy(os.Stdout, f)
			return 0
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// GC implements `microbox gc`. It cleans up after supervisors that were killed
// (SIGKILL, OOM, a crash), which leave their container dead and its cgroup,
// address, veth and port rules behind. It removes:
//
//   - dead containers, after releasing what they held
//   - cgroups and network resources whose container no longer exists
//   - container directories whose removal was interrupted
func GC(args []string) int {
	if len(args) > 0 {
		return usage("usage: microbox gc")
	}
	cs, err := list()
	if err != nil {
		return fail(err)
	}
	var errs []error
	for _, c := range cs {
		if c.State() != Dead {
			continue
		}
		if err := errors.Join(cleanupDead(c.ID), remove(c.ID)); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Printf("removed dead container %s\n", c.ID)
	}

	// A container's directory is created before its cgroup and network and
	// removed after them. So a resource whose directory doesn't exist when
	// checked here belongs to no container, not even one starting right now.
	orphan := func(id string) bool {
		_, err := os.Stat(containerDir(id))
		return errors.Is(err, fs.ErrNotExist)
	}
	groups, err := os.ReadDir(cgroups.Parent)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, g := range groups {
		if !g.IsDir() || !orphan(g.Name()) {
			continue
		}
		if err := cgroups.Lookup(g.Name()).Destroy(); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Printf("removed cgroup of %s\n", g.Name())
	}
	owners, err := network.Owners()
	errs = append(errs, err)
	for id := range owners {
		if !orphan(id) {
			continue
		}
		if err := network.Cleanup(id); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Printf("removed network of %s\n", id)
	}
	trash, err := filepath.Glob(filepath.Join(ContainersDir, ".removing-*"))
	errs = append(errs, err)
	for _, dir := range trash {
		errs = append(errs, os.RemoveAll(dir))
	}
	if err := errors.Join(errs...); err != nil {
		return fail(err)
	}
	return 0
}

// forEach runs fn on each container, like docker: it goes on after a failure,
// prints the reference of each container it handled, and fails if any failed.
func forEach(refs []string, fn func(*Container) error) int {
	code := 0
	for _, ref := range refs {
		c, err := find(ref)
		if err == nil {
			err = fn(c)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
			code = 1
			continue
		}
		fmt.Println(ref)
	}
	return code
}

func usage(s string) int {
	fmt.Fprintln(os.Stderr, s)
	return 2
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
	return 1
}
