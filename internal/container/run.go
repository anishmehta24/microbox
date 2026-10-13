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
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/anishmehta24/microbox/internal/cgroups"
	"github.com/anishmehta24/microbox/internal/network"
	"github.com/anishmehta24/microbox/internal/nsinit"
	"github.com/anishmehta24/microbox/internal/spec"
)

// Image and container directories on the host.
const (
	ImagesDir     = "/var/lib/microbox/images"     // unpacked root filesystems, one per image
	ContainersDir = "/var/lib/microbox/containers" // one per container: upper, work, merged
)

// Namespaces every container gets. The network namespace is added unless the
// container uses the host network.
const cloneFlags = syscall.CLONE_NEWUTS | // own hostname
	syscall.CLONE_NEWPID | // own process tree; the first process is PID 1
	syscall.CLONE_NEWNS | // own mount table, so pivot_root does not touch the host
	syscall.CLONE_NEWIPC | // own System V IPC and POSIX message queues
	syscall.CLONE_NEWCGROUP // /proc/self/cgroup and /sys/fs/cgroup start at the container's group

const runUsage = "usage: microbox run [--hostname NAME] [--memory SIZE] [--cpus N] [--pids N] [--net MODE] [-p HOST:CONTAINER]... IMAGE COMMAND [ARG...]"

type options struct {
	hostname string
	image    string
	argv     []string
	limits   cgroups.Limits
	net      network.Mode
	ports    portsFlag
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
	fl.StringVar(&opts.hostname, "hostname", "", "container hostname (default: the container ID)")
	memory := fl.String("memory", "", "memory limit, e.g. 256m or 1g (no swap on top)")
	fl.Float64Var(&opts.limits.CPUs, "cpus", 0, "CPU limit in CPUs, e.g. 0.5")
	fl.Int64Var(&opts.limits.Pids, "pids", 0, "maximum number of processes")
	netMode := fl.String("net", "bridge", "network: bridge, none or host")
	fl.Var(&opts.ports, "p", "publish a container TCP port as HOST:CONTAINER (repeatable)")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() < 2 {
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

	code, err := run(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "microbox: %v\n", err)
		return spec.ExitRuntimeError
	}
	return code
}

func run(opts options) (int, error) {
	if os.Geteuid() != 0 {
		return 0, errors.New("must run as root: new namespaces, mounts and pivot_root need CAP_SYS_ADMIN")
	}
	imageDir, err := resolveImage(opts.image)
	if err != nil {
		return 0, err
	}
	id, err := newID()
	if err != nil {
		return 0, err
	}
	hostname := opts.hostname
	if hostname == "" {
		hostname = id
	}
	// Containers only run in the foreground for now, so the writable layer goes
	// away with the container, like docker run --rm. M5 keeps it until rm.
	dir := filepath.Join(ContainersDir, id)
	defer os.RemoveAll(dir)
	rootfs, err := prepareRootfs(dir, imageDir)
	if err != nil {
		return 0, err
	}
	cg, err := cgroups.Create(id, opts.limits)
	if err != nil {
		return 0, fmt.Errorf("create cgroup: %w", err)
	}
	defer func() {
		if err := cg.Remove(); err != nil {
			fmt.Fprintf(os.Stderr, "microbox: remove cgroup: %v\n", err)
		}
	}()
	cgDir, err := os.Open(cg.Path)
	if err != nil {
		return 0, err
	}
	defer cgDir.Close()

	ep, err := network.Prepare(id, opts.net, opts.ports)
	if err != nil {
		return 0, fmt.Errorf("network: %w", err)
	}
	defer func() {
		if err := ep.Teardown(); err != nil {
			fmt.Fprintf(os.Stderr, "microbox: network teardown: %v\n", err)
		}
	}()
	resolvConf, err := os.ReadFile("/etc/resolv.conf")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if opts.net != network.Host {
		resolvConf = network.ResolvConf(resolvConf)
	}
	cfg := spec.Config{
		ID:         id,
		Hostname:   hostname,
		Rootfs:     rootfs,
		Args:       opts.argv,
		Env:        defaultEnv(hostname),
		Hosts:      hostsFile(hostname, ep.IP),
		ResolvConf: string(resolvConf),
	}

	// init reads its config from this pipe and does nothing until it arrives, so
	// the write is also the "go" signal: the network is attached in between.
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
	flags := uintptr(cloneFlags)
	if opts.net != network.Host {
		flags |= syscall.CLONE_NEWNET // own interfaces, routes and firewall, starting with just lo
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: flags,
		Pdeathsig:  syscall.SIGKILL, // if microbox dies, the container dies with it
		// clone3 with CLONE_INTO_CGROUP: the child starts inside its group instead
		// of being moved there after it is already running. That also makes its
		// new cgroup namespace start at that group rather than at microbox's.
		UseCgroupFD: true,
		CgroupFD:    int(cgDir.Fd()),
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

	// init is now blocked on the pipe, inside its new namespaces. Wire up its
	// network, then let it go.
	if err := ep.Attach(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, fmt.Errorf("network: %w", err)
	}
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
	if n, err := cg.OOMKills(); err == nil && n > 0 {
		fmt.Fprintf(os.Stderr, "microbox: container reached its memory limit; the OOM killer killed %d process(es)\n", n)
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
func prepareRootfs(dir, image string) (spec.Rootfs, error) {
	r := spec.Rootfs{
		Image:  image,
		Upper:  filepath.Join(dir, "upper"),
		Work:   filepath.Join(dir, "work"),
		Merged: filepath.Join(dir, "merged"),
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
