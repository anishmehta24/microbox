# MicroBox

A small Linux container runtime in Go, built to learn what happens underneath
Docker: namespaces, `pivot_root`, cgroups v2, veth networking and OCI images.

Status: all seven milestones of [PLAN.md](PLAN.md) are done (process
isolation, the container filesystem, resource limits, networking, lifecycle
management, security and images).

## Try it

Needs Linux on x86-64 (WSL2 works), root, iptables, and Go 1.25 or newer.
`make build` builds a static binary (`CGO_ENABLED=0`), which `--init` needs.

```sh
make build
sudo bin/microbox pull alpine:3.20    # or: sudo scripts/get-alpine.sh, a minirootfs as "alpine"
sudo bin/microbox run --rm alpine:3.20 /bin/sh
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

IMAGE is a pulled image, a root filesystem under `/var/lib/microbox/images`
(like `alpine` from `get-alpine.sh`), or a path to one that starts with `/` or
`./`. Without a COMMAND, the image's own runs. Options:

| Flag | Effect |
|---|---|
| `-d` | run in the background and print the container ID |
| `--rm` | remove the container when it exits; without it, it stays until `microbox rm`, as in docker |
| `--init` | PID 1 is microbox's init, which passes signals on to the command and reaps zombies |
| `-u`, `--user USER` | run as `USER`, `UID`, `USER:GROUP` or `UID:GID` from the image's `/etc/passwd` (default: the image's user) |
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

## Images

```
$ sudo bin/microbox pull nginx:alpine
e2de96513ba9 pulling 3.7 MB
d9aae54b5831 pulling 1.8 MB
...
e72112c14215 pulling 19.6 MB
pulled nginx:alpine (3dd08163706a)
$ sudo bin/microbox images
IMAGE          ID             LAYERS   SIZE
nginx:alpine   3dd08163706a   8        25.1 MB
alpine         (directory)
$ sudo bin/microbox run -d -p 8080:80 nginx:alpine
a9d4404c5d09
$ curl -s localhost:8080 | grep -o '<title>.*</title>'
<title>Welcome to nginx!</title>
$ sudo bin/microbox exec a9d4 ps -o pid,user,args
PID   USER     COMMAND
    1 root     nginx: master process nginx -g daemon off;
   30 nginx    nginx: worker process
...
$ sudo bin/microbox pull nginx:alpine
nginx:alpine is up to date
```

Names work as in docker: `alpine` is `alpine:latest` on Docker Hub,
`bitnami/redis:7` is a user's repository there, and a first part with a dot
or a port is a registry, as in `ghcr.io/owner/app:v1`. A registry on
`localhost` or `127.*` is spoken to over plain HTTP. `rmi IMAGE` forgets an
image, and `gc` then deletes the layers that no image and no existing
container uses.

How `pull` works (`internal/image`):

1. `HEAD /v2/<repo>/manifests/<tag>` with the four manifest media types
   (OCI and Docker, index and single-platform) in `Accept`. The registry
   answers 401 with a `WWW-Authenticate: Bearer realm=...,service=...,scope=...`
   challenge, so microbox first gets an anonymous token from the realm, which
   is how Docker Hub works even for public images. If the digest the HEAD
   reports is the one recorded at the last pull, the image is up to date.
   HEAD requests don't count against Docker Hub's limit on anonymous pulls.
2. `GET` the manifest. An index lists one manifest per platform, and microbox
   takes linux on its own architecture, fetched by its digest.
3. `GET /v2/<repo>/blobs/<digest>` for the config, which has the command,
   environment, working directory and user, and for each layer. Registries
   redirect blob downloads to a CDN, and Go's HTTP client drops the
   `Authorization` header when the host changes.
4. Every download is checked against its sha256 digest and size before
   anything reads it. Layers go to a temporary file first, so a layer whose
   digest doesn't match is never unpacked.
5. Each layer is unpacked into `/var/lib/microbox/oci/layers/<digest>/`,
   once, however many images share it. All paths go through an `os.Root` on
   that directory (Go 1.25), which refuses any path that resolves outside
   it, including through a symlink the layer itself created. An entry like
   `../../etc/passwd`, or a write through a symlink to `/`, fails the pull
   instead of reaching the host. A layer is unpacked into a temporary
   directory and renamed into place, so a layer directory is always complete.
6. A tar can only add files, so a layer marks deletions with special
   entries, which become what overlayfs reads:
   - `.wh.NAME` (NAME was deleted) becomes a character device 0:0 called
     NAME.
   - `.wh..wh..opq` (hide everything below this directory) becomes the
     `trusted.overlay.opaque=y` xattr on the directory.
7. The record goes to `/var/lib/microbox/oci/images/<name>.json`.

`run` then passes the layer directories, top first, as overlayfs lowerdirs
(`lowerdir=top:...:bottom`). The kernel copies mount options into one page,
which limits an image to about 40 layers. The config becomes the container's
process, as in docker:
- The command is the entrypoint plus the command given on the command line,
  or else the image's command.
- The environment is the image's, plus `HOSTNAME`, and `HOME` from the
  user's `/etc/passwd` entry.
- init creates the working directory if needed and changes to it.
- It looks the user up in the image's `/etc/passwd` and `/etc/group`, sets
  the groups, GID and UID, and execs. A user other than root ends up with no
  capabilities, as in docker. `exec` runs its command the same way.

Not supported: zstd layers, private registries (no credentials), pulling by
digest, and xattrs from layers other than the opaque marker, such as file
capabilities.

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
| `exec CONTAINER COMMAND...` | run another command inside a running container, as its user and in its working directory, exiting with its code |
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
6. Writes the config as JSON into the pipe (fd 3 in the child). It includes
   `/etc/hosts` with the container's address, and the host's `resolv.conf`
   without loopback nameservers, which the container can't reach. Then it
   waits for init to exec the command, and only after that records the
   container as running, with the host PID of its init. Until the exec,
   PID 1 is still microbox's init, and a signal from `stop` would hit init
   instead of the command. The wait uses a second pipe: init holds its write
   end with close-on-exec set, so the exec closes it and the read returns.
7. Waits, reports any OOM kills from `memory.events`, then removes the port
   rules, the proxy, the veth, the address and the cgroup, records the exit
   code, and exits with it. With `--rm` it also removes the container's
   directory.

**microbox init** (container side, `internal/nsinit`), PID 1 in the new namespace

1. Reads the config and sets the hostname.
2. Marks `/` as recursively private, so its mounts don't show up on the host.
3. Mounts an overlay on `merged`: the image's layers are the read-only lower
   layers and `upper` takes the container's writes, so the image never
   changes. The mount exists only in the container's mount namespace, so the
   kernel removes it when the container exits.
4. `pivot_root(".", ".")` into `merged`, then detaches the old root with all
   the host mounts under it.
5. Mounts `/proc` (only this PID namespace), a read-only `/sys`, a read-only
   `/sys/fs/cgroup` showing only the container's own group, and a tmpfs
   `/dev` with only null, zero, full, random, urandom and tty, plus its own
   devpts, `/dev/shm` and `/dev/mqueue`.
6. Hides the parts of `/proc` and `/sys` that belong to the host kernel, and
   makes others read-only (see Security).
7. Writes `/etc/hostname`, `/etc/hosts` and `/etc/resolv.conf`. This happens
   after the pivot, so a symlink in the image can't point the write at a host
   file.
8. Drops capabilities, sets `no_new_privs` and installs the seccomp filter.
9. Changes to the working directory and switches to the image's user (see
   Images).
10. Execs the command, which takes over PID 1. With `--init` it execs
    microbox's reaper instead, which stays PID 1 and starts the command as
    its child (see below).

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
    load microbox's binary with the container's `/`. A plain `go build` is
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

**`--init`** keeps microbox as PID 1. It starts the command as a child,
passes on every signal it receives, and reaps every child that exits,
including orphans the kernel reparents to it. When the command exits, it
exits with the same code, and the kernel kills whatever is left in the
namespace. tini (`docker run --init`) does the same.

**`gc`** cleans up after a supervisor that was killed. Its container died
with it through Pdeathsig, but the cgroup, the address, any port rules and
the record stay behind. `gc` removes dead containers and those resources,
plus any cgroup or network resource whose container directory no longer
exists. It finds port rules by the `microbox:<id>` comment they carry, and
addresses by the container ID in the reservation file, so it needs nothing
from the dead process. The same cleanup function runs when a container
exits normally.

## Security

Root in the container is still root on the host: there is no user namespace
(see PLAN.md). The rest of Docker's default protections are there, so that
root can't use the host's kernel against it:

```
$ sudo bin/microbox run --rm alpine sh -c '
    grep -E "^(CapEff|NoNewPrivs|Seccomp):" /proc/self/status
    mount -t tmpfs none /mnt; reboot -f; insmod /etc/hostname; unshare -U true
    echo x > /proc/sys/kernel/domainname; wc -c < /proc/kcore'
CapEff:	00000000a80425fb
NoNewPrivs:	1
Seccomp:	2
mount: permission denied (are you root?)
reboot: : Operation not permitted
insmod: can't insert '/etc/hostname': Operation not permitted
unshare: unshare(0x10000000): Operation not permitted
sh: can't create /proc/sys/kernel/domainname: Read-only file system
0
```

On the host the same `insmod` gets as far as `Invalid module format`, and
`unshare -U` works.

- **Capabilities.** Docker's default 14 of root's 41 (`a80425fb` above):
  enough to `chown`, bind ports below 1024, `ping` and `mknod`, but not
  `CAP_SYS_ADMIN` (mounts, most namespace work), `CAP_SYS_MODULE`,
  `CAP_SYS_BOOT`, `CAP_SYS_PTRACE`, `CAP_NET_ADMIN` or `CAP_SYS_TIME`. init
  removes the rest from the bounding set and empties the inheritable and
  ambient sets. For root, execve then computes the new capabilities as the
  bounding set plus the inheritable set, which leaves the 14.
- **no_new_privs.** execve can't grant privileges any more, for instance
  through a setuid binary in the image.
- **seccomp.** A BPF program the kernel runs on every system call
  (`internal/nsinit/seccomp_amd64.go`). It denies, with EPERM, what Docker's
  profile leaves out: mounting, `unshare` and `setns`, kernel modules,
  reboot and kexec, setting the clock, swap, the kernel keyring, `bpf`,
  `perf_event_open`, `userfaultfd`, io_uring, `open_by_handle_at` (the
  "shocker" escape) and a few obsolete calls. `clone` with a namespace flag
  fails too. `clone3` gets ENOSYS, because BPF can't read its flags from
  memory, and the C library falls back to `clone`. Calls from 32-bit or
  x32 code kill the process, since their numbers differ from the table the
  filter checks. Many of these already fail without the capabilities; the
  ones that don't are the reason for the filter: `unshare -U` (a user
  namespace needs no capability), the keyring and io_uring.
- **Masked paths.** Parts of `/proc` and `/sys` aren't namespaced and show or
  change the host: `/proc/kcore` is the host's memory, `/proc/sysrq-trigger`
  acts on the host kernel. init binds `/dev/null` over files like those and
  mounts an empty read-only tmpfs over directories, and makes `/proc/sys`,
  `/proc/sysrq-trigger`, `/proc/bus`, `/proc/fs` and `/proc/irq` read-only.
  Same lists as Docker.
- **A sealed copy of microbox.** init, exec-init and the reaper run from a
  copy of the binary in a sealed memfd, not from the file on the host. A
  process's `/proc/<pid>/exe` leads to the file it runs, and if a process
  in the container could open that link for a microbox process in the
  container, it could overwrite microbox on the host. That was
  CVE-2019-5736 in runc, which fixed it the same way.
- **`--init`'s PID 1.** Capabilities, `no_new_privs` and seccomp filters
  belong to a thread. init applies them on one locked thread and execs from
  it, so the command gets them. The reaper runs for the container's whole
  life, so instead of running it inside init, whose other Go threads would
  keep every capability, init execs a fresh copy that starts from the
  restricted thread. That exec happens after `pivot_root`, so the binary
  has to be static. The reaper also makes itself non-dumpable, which closes
  its `/proc/1/exe` and `/proc/1/mem` to processes without
  `CAP_SYS_PTRACE`.

## Layout

```
cmd/microbox/        CLI entry point: the commands, plus the hidden init, exec-init, reap and shim
internal/container/  host side: run, the shim and start/wait (run.go); the record (state.go);
                     ps, stop, rm, logs, gc (manage.go); exec (exec.go); the sealed copy
                     of the binary (binary.go); pull, images, rmi (images.go)
internal/image/      references (ref.go), the registry client and pull (registry.go),
                     unpacking layers (layer.go), the image store (store.go); imagetest/
                     is a registry in memory for the tests
internal/cgroups/    cgroup v2 group per container: limits, OOM count, kill, removal
internal/network/    bridge and NAT, IP allocation, veth + addressing, published ports, resolv.conf
internal/nsinit/     container side: hostname, exec, exec-init (init.go); overlay, pivot_root,
                     /dev, masked paths, /etc (rootfs.go); the --init reaper (reap.go);
                     capabilities and no_new_privs (security.go); the seccomp filter
                     (seccomp_amd64.go); working directory and user (user.go)
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
- the exact list of container mounts (`/sys` and the masks read-only), and an
  unchanged host mount table
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
- the command, an exec'd process and every thread of `--init`'s PID 1 have
  Docker's capabilities, `no_new_privs` and a seccomp filter
- `mount`, `reboot`, `insmod`, `unshare -U`, `unshare -m` and `date -s` fail,
  and the container keeps running
- `chown`, `mknod` and `ping` still work
- `/proc/kcore` is empty, `/proc/sys` and `/proc/sysrq-trigger` are read-only,
  `/sys/firmware` is empty
- the container can't read `/proc/1/exe` of `--init`'s PID 1, which runs the
  sealed memfd copy rather than the host's binary
- a three-layer image (alpine, then a layer adding a file, then one deleting
  a file and making a directory opaque) pulled from a registry in the test
  runs its own command with its environment, working directory and user, and
  shows only what the top layers left
- a command replaces the image's, `--user` the image's user, and an unknown
  user fails with 125
- a second pull makes no GET requests, `rmi` and `gc` remove the image and
  its layers, and a container keeps its layers after `rmi` until it is removed
- `busybox:1.36` pulled from Docker Hub runs (skipped when offline)

Unit tests cover size parsing, the limit files (including `cpu.max` rounding),
the device-number encoding against the host's own `/dev` nodes, port and mode
parsing, the `resolv.conf` filter, address allocation, parsing tagged
iptables rules, the capability mask, and the seccomp program, run through a
small BPF interpreter on sample system calls. For images:
- reference parsing and the `WWW-Authenticate` parser
- a pull from the in-memory registry: token auth, index, digests, whiteout
  devices, the opaque xattr, hard links, setuid bits and owners, and the
  overlay those layers make
- a layer with a changed byte fails its digest check and leaves nothing
- `../` paths, writes through a symlink and hard links out of the layer
  fail the pull without touching anything outside
- unused-layer removal

Without root or the image, the tests skip.

## Editing on Windows

The code uses Linux-only syscalls. Open the folder through VS Code's WSL
extension, or set `GOOS=linux` and `GOARCH=amd64` for gopls.
