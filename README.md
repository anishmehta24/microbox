# MicroBox

A small Linux container runtime in Go, built to learn what happens underneath
Docker: namespaces, `pivot_root`, cgroups v2 and veth networking.

Status: milestones 1–3 of [PLAN.md](PLAN.md) are done (process isolation, the
container filesystem and resource limits). Containers still share the host
network.

## Try it

Needs Linux (WSL2 works), root, and Go 1.22 or newer.

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
```

IMAGE is a name under `/var/lib/microbox/images` or a path to any root
filesystem. Options:

| Flag | Effect |
|---|---|
| `--hostname NAME` | hostname (default: the container ID) |
| `--memory 256m` | `memory.max`, with `memory.swap.max=0` so the limit can't be dodged by swapping |
| `--cpus 0.5` | `cpu.max` = `50000 100000`: 50 ms of CPU time per 100 ms |
| `--pids 64` | `pids.max`: forks beyond it fail |

```
$ sudo bin/microbox run --memory 32m alpine sh -c 'x=$(head -c 100m /dev/zero | tr "\0" a)'
microbox: container reached its memory limit; the OOM killer killed 1 process(es)
$ echo $?
137
```

Exit codes are the command's own, or 125 for a runtime error, 126 when the
command can't be executed, 127 when it isn't found, and 128+N when the
container is killed by signal N.

## What `microbox run` does

One binary, two processes.

**microbox run** (host side, `internal/container`)

1. Checks for root, resolves the image, makes a 12-hex-digit ID and opens a pipe.
   Creates `/var/lib/microbox/containers/<id>/` with `upper`, `work` and
   `merged`, and removes it when the container exits (like `docker run --rm`).
2. Creates the cgroup `/sys/fs/cgroup/microbox/<id>` and writes the limits.
   The parent `microbox` group never holds processes, because cgroup v2 only
   hands controllers down from groups that have none.
3. Starts `/proc/self/exe init` with `CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWIPC | CLONE_NEWCGROUP`.
   - Go is multithreaded, and a multithreaded process can't move itself into
     a new mount namespace, so microbox starts a fresh copy of itself inside
     the new namespaces instead.
   - The start uses clone3 with `CLONE_INTO_CGROUP` (Go's `UseCgroupFD`), so
     the child is born inside its cgroup, and its cgroup namespace starts at
     that group.
4. Writes the config as JSON into the pipe (fd 3 in the child). The child waits
   for it, so M4 can set up the network first.
5. Waits, reports any OOM kills from `memory.events`, removes the cgroup and
   exits with the container's exit code.

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
6. Writes `/etc/hostname`, `/etc/hosts` and a copy of the host's
   `/etc/resolv.conf`. This happens after the pivot, so a symlink in the image
   can't point the write at a host file.
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

## Layout

```
cmd/microbox/        CLI entry point, run + hidden init
internal/container/  host side: flags, image, clone flags, config pipe, signals, exit codes
internal/cgroups/    cgroup v2 group per container: limits, OOM count, removal
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
- the four namespace inodes differ from the host's
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

Unit tests cover size parsing, the limit files (including `cpu.max` rounding),
and the device-number encoding against the host's own `/dev` nodes.

Without root or the image, the tests skip.

## Editing on Windows

The code uses Linux-only syscalls. Open the folder through VS Code's WSL
extension, or set `GOOS=linux` for gopls.
