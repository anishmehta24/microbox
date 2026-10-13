# MicroBox

A small Linux container runtime in Go, built to learn what happens underneath
Docker: namespaces, `pivot_root`, cgroups v2 and veth networking.

Status: milestones 1–4 of [PLAN.md](PLAN.md) are done (process isolation, the
container filesystem, resource limits and networking). Containers run in the
foreground only; `ps`, `stop` and `exec` come in M5.

## Try it

Needs Linux (WSL2 works), root, iptables, and Go 1.22 or newer.

```sh
sudo scripts/get-alpine.sh            # Alpine minirootfs -> /var/lib/microbox/images/alpine
make build
sudo bin/microbox run alpine /bin/sh
```

```
/ # hostname
4f1c9a0b2e7d
/ # ps -o pid,comm
PID   COMMAND
    1 sh
    7 ps
/ # ip -4 addr show eth0 | grep inet
    inet 10.88.0.2/16 brd 10.88.255.255 scope global eth0
/ # wget -qO- http://example.com | grep -o '<title>.*</title>'
<title>Example Domain</title>
```

IMAGE is a name under `/var/lib/microbox/images` or a path to any root
filesystem. Options:

| Flag | Effect |
|---|---|
| `--hostname NAME` | hostname (default: the container ID) |
| `--memory 256m` | `memory.max`, with `memory.swap.max=0` so the limit can't be dodged by swapping |
| `--cpus 0.5` | `cpu.max` = `50000 100000`: 50 ms of CPU time per 100 ms |
| `--pids 64` | `pids.max`: forks beyond it fail |
| `--net bridge` | the default: own address on the `mb0` bridge, NAT to the outside |
| `--net none` | own network namespace with only `lo` |
| `--net host` | the host's network, no isolation |
| `-p 8080:80` | publish container TCP port 80 on host port 8080 (repeatable) |

```
$ sudo bin/microbox run --memory 32m alpine sh -c 'x=$(head -c 100m /dev/zero | tr "\0" a)'
microbox: container reached its memory limit; the OOM killer killed 1 process(es)
$ echo $?
137
```

A published port answers on the host's addresses, including `localhost`. On
WSL, Windows reaches it at `localhost:8080` too.

```sh
sudo bin/microbox run -p 8080:80 alpine sh -c \
  'printf "HTTP/1.0 200 OK\r\nContent-Length: 6\r\n\r\nhello\n" | nc -l -p 80 -w 1'
curl localhost:8080      # from another terminal: hello
```

Exit codes are the command's own, or 125 for a runtime error, 126 when the
command can't be executed, 127 when it isn't found, and 128+N when the
container is killed by signal N.

## What `microbox run` does

One binary, two processes.

**microbox run** (host side, `internal/container`)

1. Checks for root, resolves the image and makes a 12-hex-digit ID. Creates
   `/var/lib/microbox/containers/<id>/` with `upper`, `work` and `merged`,
   and removes it when the container exits (like `docker run --rm`).
2. Creates the cgroup `/sys/fs/cgroup/microbox/<id>` and writes the limits.
   The parent `microbox` group never holds processes, because cgroup v2 only
   hands controllers down from groups that have none.
3. Prepares the network (`internal/network`):
   - Makes sure the shared pieces exist: bridge `mb0` with 10.88.0.1/16,
     `ip_forward`, a MASQUERADE rule for traffic leaving 10.88.0.0/16, and
     FORWARD rules that let bridge traffic through.
   - Reserves the first free address by creating
     `/var/lib/microbox/network/ips/<ip>` with `O_EXCL`, which is atomic.
   - For each `-p`, adds DNAT rules: PREROUTING for traffic from outside,
     OUTPUT for the host's own connections. It also starts a proxy on
     `127.0.0.1`, because loopback traffic can't be DNATed to another
     interface (Docker's docker-proxy does the same).
4. Opens a pipe and starts `/proc/self/exe init` with `CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWIPC | CLONE_NEWCGROUP | CLONE_NEWNET`.
   - Go is multithreaded, and a multithreaded process can't move itself into
     a new mount namespace, so microbox starts a fresh copy of itself inside
     the new namespaces instead.
   - The start uses clone3 with `CLONE_INTO_CGROUP` (Go's `UseCgroupFD`), so
     the child is born inside its cgroup, and its cgroup namespace starts at
     that group.
5. While init waits on the pipe, attaches the network:
   - Creates a veth pair (a virtual cable): one end `mb<id>` plugged into
     `mb0`, the other created inside the container as `eth0`.
   - Through a netlink socket opened inside the container's namespace, it
     brings `lo` up and gives `eth0` its address and a default route via
     10.88.0.1.
6. Writes the config as JSON into the pipe (fd 3 in the child). It includes
   `/etc/hosts` with the container's address, and the host's `resolv.conf`
   without loopback nameservers, which the container can't reach.
7. Waits, reports any OOM kills from `memory.events`, then removes the port
   rules, the proxy, the veth, the address, the cgroup and the container
   directory, and exits with the container's exit code.

**microbox init** (container side, `internal/nsinit`), PID 1 in the new namespace

1. Reads the config and sets the hostname.
2. Marks `/` as recursively private, so its mounts don't show up on the host.
3. Mounts an overlay on `merged`: the image is the read-only lower layer and
   `upper` takes the container's writes, so the image never changes. The mount
   exists only in the container's mount namespace, so the kernel removes it
   when the container exits.
4. `pivot_root(".", ".")` into `merged`, then detaches the old root with all
   the host mounts under it.
5. Mounts `/proc` (only this PID namespace), a read-only `/sys`, a read-only
   `/sys/fs/cgroup` showing only the container's own group, and a tmpfs
   `/dev` with only null, zero, full, random, urandom and tty, plus its own
   devpts, `/dev/shm` and `/dev/mqueue`.
6. Writes `/etc/hostname`, `/etc/hosts` and `/etc/resolv.conf`. This happens
   after the pivot, so a symlink in the image can't point the write at a host
   file.
7. Execs the command, which takes over PID 1.

Details worth knowing:

- `Pdeathsig: SIGKILL` kills the container if microbox dies. It fires when the
  forking *thread* exits, so `run` holds `runtime.LockOSThread()`.
- Ctrl-C: the terminal sends SIGINT to the whole foreground process group, so
  the container already gets it. microbox only catches it to stay alive.
  SIGTERM and SIGHUP are forwarded.
- A namespace's PID 1 ignores every signal it has no handler for, even SIGKILL
  from inside. So `microbox run alpine sleep 30` can't be stopped with Ctrl-C,
  the same as `docker run` without `--init`.
- Deleting one end of a veth deletes both. When the container's namespace
  dies, the kernel removes `eth0` and with it `mb<id>`, but asynchronously, so
  microbox deletes `mb<id>` itself.

## Layout

```
cmd/microbox/        CLI entry point, run + hidden init
internal/container/  host side: flags, image, clone flags, config pipe, signals, exit codes
internal/cgroups/    cgroup v2 group per container: limits, OOM count, removal
internal/network/    bridge and NAT, IP allocation, veth + addressing, published ports, resolv.conf
internal/nsinit/     container side: hostname, exec (init.go); overlay, pivot_root, /dev, /etc (rootfs.go)
internal/spec/       config and exit codes shared by both sides
e2e/                 tests that run the real binary
scripts/get-alpine.sh
```

## Tests

`make test` runs `go vet`, the gofmt check, and then the tests as root. The e2e
tests build the binary and run it against the alpine image. They check:

- the hostname, and that the command is PID 1
- `ps` sees only the container
- all six namespace inodes differ from the host's
- the exact list of container mounts (`/sys` read-only), and an unchanged host mount table
- `/dev` holds exactly the default devices and links
- `/etc/hostname`, `/etc/hosts` and `/etc/resolv.conf`
- writes and deletes stay in the container: the image and the next container don't
  see them, parallel containers don't see each other's, and the container
  directory is gone afterwards
- exit codes, and SIGKILL from inside vs from the host
- `/proc/self/cgroup` is `0::/`, and the limits (or `max`) are readable inside
- a 100 MB shell variable under `--memory 32m` is OOM-killed with exit 137
- `--pids 8` peaks at 8 processes and counts the refused forks
- a one-second busy loop under `--cpus 0.2` is throttled to well under 400 ms of CPU
- bad flag values exit 2, and no cgroup is left behind
- the container has only `eth0` and `lo`, an address in 10.88.0.0/16 matching
  `/etc/hosts`, and a default route via the bridge
- two containers ping each other and talk over TCP
- the internet is reachable through NAT, DNS included (skipped when the host is offline)
- `-p` serves through both `127.0.0.1` and the host's bridge address, and its
  rules and listener are gone after exit
- a port that is already taken fails cleanly, with nothing left behind
- `--net none` has only a working `lo`, and `--net host` shares the host's namespace
- no veth or IP reservation is left behind

Unit tests cover size parsing, the limit files (including `cpu.max` rounding),
the device-number encoding against the host's own `/dev` nodes, port and mode
parsing, the `resolv.conf` filter, and address allocation.

Without root or the image, the tests skip.

## Editing on Windows

The code uses Linux-only syscalls. Open the folder through VS Code's WSL
extension, or set `GOOS=linux` for gopls.
