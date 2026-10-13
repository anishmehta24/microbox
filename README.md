# MicroBox

A small Linux container runtime in Go, built to learn what happens underneath
Docker: namespaces, `pivot_root`, cgroups v2 and veth networking.

Status: milestone 1 of [PLAN.md](PLAN.md) is done (process isolation and root
filesystem). There are no resource limits yet, and containers share the host
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

`--hostname NAME` sets the hostname; the default is the container ID. IMAGE is
a name under `/var/lib/microbox/images` or a path to any root filesystem.

Exit codes are the command's own, or 125 for a runtime error, 126 when the
command can't be executed, 127 when it isn't found, and 128+N when the
container is killed by signal N.

## What `microbox run` does

One binary, two processes.

**microbox run** (host side, `internal/container`)

1. Checks for root, resolves the image, makes a 12-hex-digit ID and opens a pipe.
2. Starts `/proc/self/exe init` with `CLONE_NEWUTS | CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWIPC`.
   Go is multithreaded, and a multithreaded process can't move itself into a
   new mount namespace, so microbox starts a fresh copy of itself inside the
   new namespaces instead.
3. Writes the config as JSON into the pipe (fd 3 in the child). The child waits
   for it, so later milestones can set up the cgroup and network first.
4. Waits, then exits with the container's exit code.

**microbox init** (container side, `internal/nsinit`), PID 1 in the new namespace

1. Reads the config and sets the hostname.
2. Marks `/` as recursively private, so its mounts don't show up on the host.
3. Bind-mounts the rootfs onto itself (`pivot_root` needs a mount point), then
   `pivot_root(".", ".")` and detaches the old root with all the host mounts
   under it.
4. Mounts a fresh `/proc`, which lists only this PID namespace.
5. Execs the command, which takes over PID 1.

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
internal/container/  host side: image, clone flags, config pipe, signals, exit codes
internal/nsinit/     container side: hostname, mounts, pivot_root, exec
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
- the container's only mounts are `/` and `/proc`, and the host mount table is unchanged
- exit codes, and SIGKILL from inside vs from the host

Without root or the image, the tests skip.

## Editing on Windows

The code uses Linux-only syscalls. Open the folder through VS Code's WSL
extension, or set `GOOS=linux` for gopls.
