// Package container is the host side of microbox: it prepares a container,
// starts init inside new namespaces, waits for it, and implements the
// commands that manage containers afterwards.
package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/anishmehta24/microbox/internal/cgroups"
	"github.com/anishmehta24/microbox/internal/image"
	"github.com/anishmehta24/microbox/internal/network"
	"github.com/anishmehta24/microbox/internal/nsinit"
	"github.com/anishmehta24/microbox/internal/spec"
)

// Image and container directories on the host.
const (
	ImagesDir     = "/var/lib/microbox/images"     // unpacked root filesystems, one per image
	ContainersDir = "/var/lib/microbox/containers" // one per container: record, layers, log
)

// ShimCommand is the hidden argument for the supervisor of a detached container.
const ShimCommand = "shim"

// Namespaces every container gets. The network namespace is added unless the
// container uses the host network.
const cloneFlags = syscall.CLONE_NEWUTS | // own hostname
	syscall.CLONE_NEWPID | // own process tree; the first process is PID 1
	syscall.CLONE_NEWNS | // own mount table, so pivot_root does not touch the host
	syscall.CLONE_NEWIPC | // own System V IPC and POSIX message queues
	syscall.CLONE_NEWCGROUP // /proc/self/cgroup and /sys/fs/cgroup start at the container's group

const runUsage = "usage: microbox run [-d] [--rm] [--init] [--user USER] [--hostname NAME] [--memory SIZE] [--cpus N] [--pids N] [--net MODE] [-p HOST:CONTAINER]... IMAGE [COMMAND [ARG...]]"

type options struct {
	hostname   string
	user       string
	image      string
	argv       []string // empty: the image's command
	limits     cgroups.Limits
	net        network.Mode
	ports      portsFlag
	detach     bool
	autoRemove bool
	init       bool
}

// portsFlag collects repeated -p flags.
type portsFlag []network.PortMapping

func (p *portsFlag) String() string { return fmt.Sprint([]network.PortMapping(*p)) }

func (p *portsFlag) Set(s string) error {
	m, err := network.ParsePort(s)
	if err == nil {
		*p = append(*p, m)
	}
	return err
}

// Run implements `microbox run` and returns the exit code for microbox.
func Run(args []string) int {
	var opts options
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	fl.BoolVar(&opts.detach, "d", false, "run in the background and print the container ID")
	fl.BoolVar(&opts.autoRemove, "rm", false, "remove the container when it exits")
	fl.BoolVar(&opts.init, "init", false, "run an init as PID 1 that forwards signals and reaps zombies")
	fl.StringVar(&opts.hostname, "hostname", "", "container hostname (default: the container ID)")
	for _, name := range []string{"user", "u"} {
		fl.StringVar(&opts.user, name, "", "run as USER, UID, USER:GROUP or UID:GID (default: the image's user, or root)")
	}
	memory := fl.String("memory", "", "memory limit, e.g. 256m or 1g (no swap on top)")
	fl.Float64Var(&opts.limits.CPUs, "cpus", 0, "CPU limit in CPUs, e.g. 0.5")
	fl.Int64Var(&opts.limits.Pids, "pids", 0, "maximum number of processes")
	netMode := fl.String("net", "bridge", "network: bridge, none or host")
	fl.Var(&opts.ports, "p", "publish a container TCP port as HOST:CONTAINER (repeatable)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() < 1 {
		fmt.Fprintln(os.Stderr, runUsage)
		return 2
	}
	opts.image, opts.argv = fl.Arg(0), fl.Args()[1:]

	var err error
	opts.net, err = network.ParseMode(*netMode)
	if err == nil && len(opts.ports) > 0 && opts.net != network.Bridge {
		err = fmt.Errorf("-p needs --net bridge")
	}
	if err == nil && *memory != "" {
		opts.limits.MemoryBytes, err = cgroups.ParseBytes(*memory)
	}
	// Written so that NaN fails too: every comparison with NaN is false. 0.01 is
	// the kernel's 1 ms minimum quota; the top end is Docker's limit.
	if c := opts.limits.CPUs; err == nil && c != 0 && !(c >= 0.01 && c <= float64(runtime.NumCPU())) {
		err = fmt.Errorf("--cpus must be between 0.01 and %d, the number of CPUs", runtime.NumCPU())
	}
	if err == nil && opts.limits.Pids < 0 {
		err = fmt.Errorf("--pids must be positive")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
		return 2
	}

	c, err := create(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
		return spec.ExitRuntimeError
	}
	if opts.detach {
		return detach(c)
	}
	return foreground(c)
}

// create makes the container's directory and record. Nothing runs yet.
func create(opts options) (*Container, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("must run as root: new namespaces, mounts and pivot_root need CAP_SYS_ADMIN")
	}
	if opts.init {
		if err := checkStatic(); err != nil {
			return nil, err
		}
	}
	img, err := resolveImage(opts.image)
	if err != nil {
		return nil, err
	}
	// As in docker: the image's entrypoint, then the command given or else
	// the image's command.
	args := append(slices.Clone(img.config.Entrypoint), opts.argv...)
	if len(opts.argv) == 0 {
		args = append(args, img.config.Cmd...)
	}
	if len(args) == 0 {
		return nil, fmt.Errorf("no command given, and image %s has none", opts.image)
	}
	user := opts.user
	if user == "" {
		user = img.config.User
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	hostname := opts.hostname
	if hostname == "" {
		hostname = id
	}
	self, err := procOf(os.Getpid())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(ContainersDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(containerDir(id), 0o755); err != nil {
		return nil, err
	}
	c := &Container{
		ID:      id,
		Image:   opts.image,
		Created: time.Now(),
		Config: spec.Config{
			ID:       id,
			Hostname: hostname,
			Process: spec.Process{
				Args: args,
				Env:  containerEnv(img.config.Env, hostname),
				Cwd:  img.config.WorkingDir,
				User: user,
			},
			Init: opts.init,
		},
		Limits:     opts.limits,
		Net:        opts.net,
		Ports:      opts.ports,
		Detached:   opts.detach,
		AutoRemove: opts.autoRemove,
		Status:     Created,
		Supervisor: self,
	}
	c.Config.Rootfs, err = prepareRootfs(containerDir(id), img.lower)
	if err == nil {
		err = c.save()
	}
	if err != nil {
		remove(id)
		return nil, err
	}
	return c, nil
}

// foreground runs the container attached to the terminal, with this process
// as its supervisor.
func foreground(c *Container) int {
	// Pdeathsig fires when the thread that forked the child exits, not the whole
	// process. Pin this goroutine to its thread so the Go scheduler cannot retire
	// that thread while the container is running.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	in, err := start(c, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		remove(c.ID)
		fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
		return spec.ExitRuntimeError
	}
	return in.wait()
}

// detach hands the container to a supervisor of its own: a copy of microbox in
// a new session, the job containerd-shim does for Docker. It has no terminal,
// so closing this one doesn't send it SIGHUP, and it keeps running after this
// command returns, which happens as soon as the container is running.
func detach(c *Container) int {
	fail := func(err error) int {
		remove(c.ID)
		fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
		return spec.ExitRuntimeError
	}
	// The container writes its output straight into the log file.
	logFile, err := os.OpenFile(logPath(c.ID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fail(err)
	}
	defer logFile.Close()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return fail(err)
	}
	defer devNull.Close()
	// The shim reports on this pipe whether the container started.
	pr, pw, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	defer pr.Close()

	cmd := exec.Command("/proc/self/exe", ShimCommand, c.ID)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, logFile, logFile
	cmd.ExtraFiles = []*os.File{pw} // shimReadyFD
	cmd.Dir = "/"                   // don't keep the caller's working directory busy
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	pw.Close()
	if err != nil {
		return fail(fmt.Errorf("start supervisor: %w", err))
	}
	msg, _ := io.ReadAll(pr)
	if string(msg) != shimReady {
		cmd.Wait()
		if len(msg) == 0 {
			msg = []byte("the supervisor exited without starting the container")
		}
		return fail(errors.New(string(msg)))
	}
	// The shim is not waited for. When this process exits, the kernel
	// reparents it to the host's init, which collects it in the end.
	fmt.Println(c.ID)
	return 0
}

// The shim writes shimReady to the pipe on shimReadyFD once the container
// runs, or the error if it couldn't start it.
const (
	shimReady   = "ok"
	shimReadyFD = 3
)

// Shim is the supervisor of a detached container, started by detach. Its
// stdout and stderr are the container's log file.
func Shim(args []string) int {
	runtime.LockOSThread() // for Pdeathsig, see foreground
	ready := os.NewFile(shimReadyFD, "ready")
	if len(args) != 1 {
		fmt.Fprintf(ready, "usage: microbox %s ID", ShimCommand)
		return 2
	}
	c, err := load(args[0])
	if err != nil {
		fmt.Fprint(ready, err)
		return spec.ExitRuntimeError
	}
	in, err := start(c, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		remove(c.ID)
		fmt.Fprint(ready, err)
		return spec.ExitRuntimeError
	}
	ready.WriteString(shimReady)
	ready.Close()
	return in.wait()
}

// instance is a started container and what start set up for it.
type instance struct {
	c    *Container
	cmd  *exec.Cmd
	cg   *cgroups.Cgroup
	ep   *network.Endpoint
	sigs chan os.Signal
}

// start sets up the cgroup and the network, starts init and lets it run. The
// calling goroutine must be locked to its thread, see foreground. If start
// fails, it has undone everything.
func start(c *Container, stdin, stdout, stderr *os.File) (_ *instance, err error) {
	in := &instance{c: c}
	defer func() {
		if err == nil {
			return
		}
		if in.cmd != nil {
			in.cmd.Process.Kill()
			in.cmd.Wait()
		}
		in.release()
	}()
	if c.Supervisor, err = procOf(os.Getpid()); err != nil {
		return nil, err
	}
	if in.cg, err = cgroups.Create(c.ID, c.Limits); err != nil {
		return nil, fmt.Errorf("create cgroup: %w", err)
	}
	cgDir, err := os.Open(in.cg.Path)
	if err != nil {
		return nil, err
	}
	defer cgDir.Close()

	if in.ep, err = network.Prepare(c.ID, c.Net, c.Ports); err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	resolvConf, err := os.ReadFile("/etc/resolv.conf")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if c.Net != network.Host {
		resolvConf = network.ResolvConf(resolvConf)
	}
	c.Config.Hosts = hostsFile(c.Config.Hostname, in.ep.IP)
	c.Config.ResolvConf = string(resolvConf)

	// init reads its config from this pipe and does nothing until it arrives, so
	// the write is also the "go" signal: the network is attached in between.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer pw.Close()
	// init closes its end of this one when the command runs, see spec.StartedFD.
	started, startedW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer started.Close()
	exe, err := sealedCopy()
	if err != nil {
		return nil, err
	}
	defer exe.Close()

	// The Go runtime is multithreaded, and a multithreaded process cannot move
	// itself into a new mount namespace. So instead of unsharing in place,
	// microbox starts a fresh copy of itself with clone flags.
	cmd := exec.Command(exePath, nsinit.Command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.ExtraFiles = []*os.File{exe, pr, startedW} // spec.ExeFD, InitPipeFD, StartedFD
	cmd.Env = []string{}                           // the container env travels in the config, keep the host's out
	flags := uintptr(cloneFlags)
	if c.Net != network.Host {
		flags |= syscall.CLONE_NEWNET // own interfaces, routes and firewall, starting with just lo
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: flags,
		Pdeathsig:  syscall.SIGKILL, // if the supervisor dies, the container dies with it
		// clone3 with CLONE_INTO_CGROUP: the child starts inside its group instead
		// of being moved there after it is already running. That also makes its
		// new cgroup namespace start at that group rather than at microbox's.
		UseCgroupFD: true,
		CgroupFD:    int(cgDir.Fd()),
	}

	// Registered before Start: signals Go handles are reset to the default in the
	// child, so the container does not inherit this.
	in.sigs = make(chan os.Signal, 4)
	signal.Notify(in.sigs, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGHUP)

	err = cmd.Start()
	pr.Close() // the child has its own copies
	startedW.Close()
	if err != nil {
		return nil, fmt.Errorf("start init: %w", err)
	}
	in.cmd = cmd
	go forwardSignals(in.sigs, cmd.Process)

	// init is now blocked on the pipe, inside its new namespaces. Wire up its
	// network, then let it go.
	if err := in.ep.Attach(cmd.Process.Pid); err != nil {
		return nil, fmt.Errorf("network: %w", err)
	}
	if c.Pid1, err = procOf(cmd.Process.Pid); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(pw).Encode(c.Config); err != nil {
		return nil, fmt.Errorf("send config to init: %w", err)
	}
	// Until the command runs, PID 1 is still microbox's init. A command that
	// fails to start lands here as well, and wait records its exit code.
	if _, err := io.Copy(io.Discard, started); err != nil {
		return nil, err
	}
	c.Status = Running
	if err := c.save(); err != nil {
		return nil, err
	}
	return in, nil
}

// wait waits for the container to exit, releases its cgroup and network, and
// records the exit. It returns the exit code for microbox.
func (in *instance) wait() int {
	code := spec.ExitRuntimeError
	err := in.cmd.Wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		fmt.Fprintf(os.Stderr, "microbox: wait for container: %v\n", err)
	} else {
		code = exitCode(in.cmd.ProcessState)
	}
	if n, err := in.cg.OOMKills(); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "microbox: container reached its memory limit; the OOM killer killed %d process(es)\n", n)
	}
	in.release()

	c := in.c
	c.Status, c.ExitCode, c.Finished = Exited, code, time.Now()
	if err := c.save(); err != nil {
		fmt.Fprintf(os.Stderr, "microbox: record exit: %v\n", err)
	}
	if c.AutoRemove {
		if err := remove(c.ID); err != nil {
			fmt.Fprintf(os.Stderr, "microbox: remove container: %v\n", err)
		}
	}
	return code
}

// release removes what start set up on the host, once the container has no
// processes left. Failures are reported rather than returned: microbox gc can
// retry them later.
func (in *instance) release() {
	if in.sigs != nil {
		signal.Stop(in.sigs)
	}
	if in.ep != nil {
		if err := in.ep.Teardown(); err != nil {
			fmt.Fprintf(os.Stderr, "microbox: network teardown: %v\n", err)
		}
	}
	if in.cg != nil {
		if err := in.cg.Remove(); err != nil {
			fmt.Fprintf(os.Stderr, "microbox: remove cgroup: %v\n", err)
		}
	}
}

// forwardSignals relays SIGTERM and SIGHUP to the container. SIGINT and SIGQUIT
// come from the terminal, which already sends them to the whole foreground
// process group, container included. microbox only has to survive them.
//
// A PID 1 without a handler ignores them anyway (same as docker run without
// --init), which is what --init is for.
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

// resolvedImage is what a container is built from: its layer directories, top
// first, and how to run it.
type resolvedImage struct {
	lower  []string
	config image.Config
}

// resolveImage finds the image a name refers to. It is, in this order:
//   - a path to a root filesystem, if it starts with / or .
//   - an image pulled with microbox pull, such as alpine:3.20
//   - a root filesystem under ImagesDir, such as the one get-alpine.sh unpacks
func resolveImage(name string) (resolvedImage, error) {
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, ".") {
		dir, err := rootfsDir(name)
		return resolvedImage{lower: []string{dir}}, err
	}
	if _, err := image.ParseReference(name); err == nil {
		img, err := image.Lookup(name)
		if err == nil {
			return resolvedImage{img.LowerDirs(), img.Config}, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return resolvedImage{}, err
		}
	}
	dir := filepath.Join(ImagesDir, name)
	if _, err := os.Stat(dir); err != nil {
		return resolvedImage{}, fmt.Errorf("image %q not found: pull it with microbox pull, or unpack a root filesystem at %s (scripts/get-alpine.sh does that for alpine)", name, dir)
	}
	dir, err := rootfsDir(dir)
	return resolvedImage{lower: []string{dir}}, err
}

// rootfsDir checks a directory used as a whole root filesystem.
func rootfsDir(path string) (string, error) {
	dir, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("image: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("image %s is not a directory", dir)
	}
	// overlayfs splits its options on ',' and lowerdir on ':', and reads '\'
	// as an escape, so /tmp/a\b would be looked up as /tmp/ab.
	if strings.ContainsAny(dir, `,:\`) {
		return "", fmt.Errorf(`image path %s can't contain ',', ':' or '\', which have special meaning in overlayfs options`, dir)
	}
	return dir, nil
}

// prepareRootfs creates the container's overlay directories. init mounts the
// overlay itself, inside its own mount namespace, so the host never sees that
// mount and the kernel removes it when the container's last process exits.
func prepareRootfs(dir string, lower []string) (spec.Rootfs, error) {
	r := spec.Rootfs{
		Lower:  lower,
		Upper:  filepath.Join(dir, "upper"),
		Work:   filepath.Join(dir, "work"),
		Merged: filepath.Join(dir, "merged"),
	}
	// The kernel copies mount options into one page, so that is as many
	// layers as one overlay mount can stack: about 40 with these paths.
	if n := len("lowerdir=,upperdir=,workdir=") + len(strings.Join(r.Lower, ":")) + len(r.Upper) + len(r.Work); n >= os.Getpagesize() {
		return r, fmt.Errorf("the image has %d layers, more than one overlay mount can take", len(r.Lower))
	}
	// 0755 matters for upper: the root directory of the overlay takes its owner
	// and mode from the upper layer.
	for _, d := range []string{r.Upper, r.Work, r.Merged} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return r, err
		}
	}
	return r, nil
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// hostsFile maps the hostname to the container's address, or to 127.0.1.1
// (the Debian convention) when it has none.
func hostsFile(hostname string, ip net.IP) string {
	self := "127.0.1.1"
	if ip != nil {
		self = ip.String()
	}
	return "127.0.0.1\tlocalhost\n" +
		"::1\tlocalhost ip6-localhost ip6-loopback\n" +
		self + "\t" + hostname + "\n"
}

// containerEnv is the image's environment plus what docker adds: a PATH if
// the image sets none, HOSTNAME, and the caller's TERM. init adds HOME, from
// the user's entry in the image's /etc/passwd.
func containerEnv(imageEnv []string, hostname string) []string {
	env := setEnv([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, imageEnv...)
	env = setEnv(env, "HOSTNAME="+hostname)
	if term := os.Getenv("TERM"); term != "" {
		env = setEnv(env, "TERM="+term)
	}
	return env
}

// setEnv sets each KEY=VALUE in env, replacing an earlier value for KEY.
func setEnv(env []string, kvs ...string) []string {
	for _, kv := range kvs {
		key, _, _ := strings.Cut(kv, "=")
		i := slices.IndexFunc(env, func(e string) bool { return strings.HasPrefix(e, key+"=") })
		if i >= 0 {
			env[i] = kv
		} else {
			env = append(env, kv)
		}
	}
	return env
}
