# MicroBox

A small Linux container runtime in Go, built to learn what happens underneath
Docker: namespaces, `pivot_root`, cgroups v2 and veth networking.

Status: milestones 1–5 of [PLAN.md](PLAN.md) are done (process isolation, the
container filesystem, resource limits, networking and lifecycle management).

## Try it

Needs Linux (WSL2 works), root, iptables, and Go 1.22 or newer.

```sh
sudo scripts/get-alpine.sh            # Alpine minirootfs -> /var/lib/microbox/images/alpine
make build
sudo bin/microbox run --rm alpine /bin/sh
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
| `-d` | run in the background and print the container ID |
| `--rm` | remove the container when it exits; without it, it stays until `microbox rm`, as in docker |
| `--init` | PID 1 is microbox's init, which passes signals on to the command and reaps zombies |
| `--hostname NAME` | hostname (default: the container ID) |
| `--memory 256m` | `memory.max`, with `memory.swap.max=0` so the limit can't be dodged by swapping |
| `--cpus 0.5` | `cpu.max` = `50000 100000`: 50 ms of CPU time per 100 ms |
| `--pids 64` | `pids.max`: forks beyond it fail |
| `--net bridge` | the default: own address on the `mb0` bridge, NAT to the outside |
| `--net none` | own network namespace with only `lo` |
| `--net host` | the host's network, no isolation |
| `-p 8080:80` | publish container TCP port 80 on host port 8080 (repeatable) |

```
$ sudo bin/microbox run --rm --memory 32m alpine sh -c 'x=$(head -c 100m /dev/zero | tr "\0" a)'
microbox: container reached its memory limit; the OOM killer killed 1 process(es)
$ echo $?
137
```

Exit codes are the command's own, or 125 for a runtime error, 126 when the
command can't be executed, 127 when it isn't found, and 128+N when the
container is killed by signal N.

## Managing containers

```
$ sudo bin/microbox run -d -p 8080:80 alpine sh -c 'while :; do
    printf "HTTP/1.0 200 OK\r\nContent-Length: 6\r\n\r\nhello\n" | nc -l -p 80 >/dev/null; echo served; done'
cd6132790b2e
$ sudo bin/microbox ps
CONTAINER ID   IMAGE    COMMAND                  CREATED   STATUS   PORTS
cd6132790b2e   alpine   "sh -c while :; do p…"   0s ago    Up 0s    8080->80/tcp
$ curl localhost:8080
hello
$ sudo bin/microbox logs cd6132790b2e
served
$ sudo bin/microbox exec cd61 ps -o pid,comm
PID   COMMAND
    1 sh
   10 nc
   12 ps
$ time sudo bin/microbox stop cd61
cd61
real    0m10.135s
$ sudo bin/microbox ps -a
CONTAINER ID   IMAGE    COMMAND                  CREATED   STATUS                PORTS
cd6132790b2e   alpine   "sh -c while :; do p…"   10s ago   Exited (137) 0s ago   8080->80/tcp
$ sudo bin/microbox rm cd61
cd61
```

A container is named by its ID or any unique prefix of it. A published port
answers on the host's addresses, including `localhost`; on WSL, Windows
reaches it at `localhost:8080` too.

The `stop` took the full 10 seconds: `sh` as PID 1 ignores SIGTERM (see
below), so `stop` killed it after the timeout, hence the 137. With `--init`
the same stop takes milliseconds and the exit code is 143.

| Command | What it does |
|---|---|
| `ps [-a]` | running containers, or all of them |
| `stop [-t SECONDS] CONTAINER...` | SIGTERM to PID 1, then SIGKILL to every process in the container after the timeout (default 10) |
| `rm [-f] CONTAINER...` | remove stopped containers; `-f` kills running ones first |
| `exec CONTAINER COMMAND...` | run another command inside a running container, exiting with its code |
| `logs [-f] CONTAINER` | the output of a `-d` container; `-f` follows it until the container exits |
| `gc` | clean up after microbox processes that were killed (see below) |

## What `microbox run` does

One binary, two processes.

**microbox run** (host side, `internal/container`)

1. Checks for root, resolves the image and makes a 12-hex-digit ID. Creates
   `/var/lib/microbox/containers/<id>/` with `upper`, `work` and `merged`,
   and the container's record, `state.json`. With `-d` it now hands the
   container to a supervisor process of its own (see the next section) and
   that process does the rest.
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
6. Records the container as running, with the host PID of its init, and
   writes the config as JSON into the pipe (fd 3 in the child). It includes
   `/etc/hosts` with the container's address, and the host's `resolv.conf`
   without loopback nameservers, which the container can't reach.
7. Waits, reports any OOM kills from `memory.events`, then removes the port
   rules, the proxy, the veth, the address and the cgroup, records the exit
   code, and exits with it. With `--rm` it also removes the container's
   directory.

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
7. Execs the command, which takes over PID 1. With `--init` it starts the
   command as its child instead and stays PID 1 (see below).

Details worth knowing:

- `Pdeathsig: SIGKILL` kills the container if its supervisor dies. It fires
  when the forking *thread* exits, so the supervisor holds
  `runtime.LockOSThread()`.
- Ctrl-C: the terminal sends SIGINT to the whole foreground process group, so
  the container already gets it. microbox only catches it to stay alive.
  SIGTERM and SIGHUP are forwarded.
- A namespace's PID 1 ignores every signal it has no handler for, even SIGKILL
  from inside. So `microbox run alpine sleep 30` can't be stopped with Ctrl-C,
  the same as `docker run` without `--init`.
- Deleting one end of a veth deletes both. When the container's namespace
  dies, the kernel removes `eth0` and with it `mb<id>`, but asynchronously, so
  microbox deletes `mb<id>` itself.

## How the lifecycle works

**The record.** `state.json` holds the container's config and its status:
created, then running, then exited with an exit code. It is written to a
temporary file and renamed into place, so `ps` never reads half of one. The
process that writes it while the container runs is its *supervisor*: the
`run` command itself in the foreground, or the shim with `-d`. The record
keeps the PIDs of the supervisor and of the container's init together with
their start times from `/proc/<pid>/stat`, because PIDs get reused. A
container whose supervisor is gone without recording an exit shows up as
*dead*.

**`run -d`** starts `microbox shim <id>` with `Setsid`. The shim is a copy of
microbox in a session of its own and with no terminal, the job
containerd-shim does for Docker. Closing the terminal sends SIGHUP to its
session, which the shim isn't in. The shim starts the container exactly
like a foreground `run` does, with the container's output going straight
into `containers/<id>/log`. It reports "started" or the error back over a
pipe, and `run -d` exits as soon as it hears back. The shim stays behind to
wait for the container, hold the published-port proxies, and clean up.

**`stop`** sends SIGTERM to the container's PID 1 and waits until the
supervisor has recorded the exit. After the timeout it writes `1` to
`cgroup.kill`, which SIGKILLs every process in the cgroup at once, including
ones `exec` started. When `stop` returns, the cgroup, veth, address and port
rules are gone.

**`rm`** renames the container's directory to `.removing-<id>` and then
deletes it, so it disappears from `ps` at once, and `gc` can finish a
removal that was interrupted.

**`exec`** runs `microbox exec-init` inside the container, and that process
execs the command. Joining the namespaces is split between the two:

- `exec` joins the PID namespace itself. `setns(CLONE_NEWPID)` never moves
  the caller, only the children it forks afterwards. It locks its goroutine
  to one OS thread first, because setns only changes the calling thread.
- `exec-init` joins the others (IPC, UTS, network, cgroup, mount) after it
  has started. This is the order that works:
  - If `exec` joined the mount namespace before forking, the kernel would
    load microbox's binary with the container's `/`. A default build is
    dynamically linked (the `net` package pulls in cgo), so the kernel
    looks for `/lib64/ld-linux-x86-64.so.2` in the image. Alpine doesn't
    have it, so the exec fails with ENOENT.
  - `exec-init` is cloned straight into the container's cgroup
    (`CLONE_INTO_CGROUP`, as in `run`). The kernel only allows that from
    outside the container's cgroup namespace, because cgroup2 is mounted
    with `nsdelegate`: a process inside a cgroup namespace may only move
    processes between groups inside it.
  - Joining a mount namespace needs a thread that doesn't share its root
    and working directory with other threads. Go's threads do share them,
    which is why runc's nsexec does this in C before the Go runtime starts.
    `unshare(CLONE_FS)` on the locked thread gives it its own copy, and then
    `setns` works from Go.

**`--init`** keeps microbox's init as PID 1. It starts the command as a
child, passes on every signal it receives, and reaps every child that
exits, including orphans the kernel reparents to it. When the command
exits, init exits with the same code, and the kernel kills whatever is left
in the namespace. tini (`docker run --init`) does the same.

**`gc`** cleans up after a supervisor that was killed. Its container died
with it through Pdeathsig, but the cgroup, the address, any port rules and
the record stay behind. `gc` removes dead containers and those resources,
plus any cgroup or network resource whose container directory no longer
exists. It finds port rules by the `microbox:<id>` comment they carry, and
addresses by the container ID in the reservation file, so it needs nothing
from the dead process. The same cleanup function runs when a container
exits normally.

## Layout

```
cmd/microbox/        CLI entry point: the commands, plus the hidden init, exec-init and shim
internal/container/  host side: run, the shim and start/wait (run.go); the record (state.go);
                     ps, stop, rm, logs, gc (manage.go); exec (exec.go)
internal/cgroups/    cgroup v2 group per container: limits, OOM count, kill, removal
internal/network/    bridge and NAT, IP allocation, veth + addressing, published ports, resolv.conf
internal/nsinit/     container side: hostname, exec, exec-init (init.go); overlay, pivot_root,
                     /dev, /etc (rootfs.go); the --init reaper (reap.go)
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
- `run -d` prints an ID, `ps` lists the container and `logs` has its output;
  it keeps running after the terminal that started it is gone, and its
  supervisor has its own session and no terminal
- `stop` kills a `sleep` as PID 1 after the timeout (exit 137), while with
  `--init` the SIGTERM gets through at once (exit 143)
- without `--init` an orphan stays a zombie under PID 1; with it, it is reaped
- `exec` gets the container's six namespaces and cgroup, and passes on exit codes
  (3, 126, 127); exec into a stopped container fails with 125
- after `stop` and `rm`: no cgroup, veth, address, port rule, mount or directory
- `rm` refuses a running container, `rm -f` kills it, and a second `rm` fails cleanly
- without `--rm` a container stays as exited until `rm`
- `logs -f` follows until the container exits
- killing the supervisor leaves a dead container, and `gc` removes it and its leftovers
- a `-d` container that fails to start leaves nothing behind

Unit tests cover size parsing, the limit files (including `cpu.max` rounding),
the device-number encoding against the host's own `/dev` nodes, port and mode
parsing, the `resolv.conf` filter, address allocation and parsing tagged
iptables rules.

Without root or the image, the tests skip.

## Editing on Windows

The code uses Linux-only syscalls. Open the folder through VS Code's WSL
extension, or set `GOOS=linux` for gopls.
