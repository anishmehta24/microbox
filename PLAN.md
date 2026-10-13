# Plan

Each milestone ends with e2e tests that prove the "done when" line.

## M1: namespaces and root filesystem (done)

UTS, PID, mount and IPC namespaces through the `/proc/self/exe init` re-exec,
the config pipe on fd 3, `pivot_root(".", ".")`, `/proc`, signal handling and
exit codes.

Done when: `microbox run alpine /bin/sh` shows its own hostname, and `ps` lists
the shell as PID 1 and nothing from the host.

## M2: filesystem (done)

- An overlayfs per container: the image as lowerdir, with upper and work under
  `/var/lib/microbox/containers/<id>/`. init mounts it inside the container's
  mount namespace, so no unmount is needed on the host. The directory is
  removed on exit until M5 adds `rm`.
- `/dev` as tmpfs with null, zero, full, random, urandom and tty, plus the
  fd/stdin/stdout/stderr links, its own devpts with `/dev/ptmx`, `/dev/shm`
  and `/dev/mqueue`.
- `/sys` read-only. `/etc/hostname`, `/etc/hosts` and the host's
  `/etc/resolv.conf` are written after the pivot, so image symlinks can't
  redirect them to the host.

Done when: a file written in one container doesn't appear in the image or in
another container, and `ls > /dev/null` works.

## M3: cgroups v2 (done)

- `/sys/fs/cgroup/microbox/<id>`, with the parent `microbox/` cgroup kept empty
  (the "no internal processes" rule).
- Flags `--memory` (`memory.max` plus `memory.swap.max=0`), `--cpus` (`cpu.max`,
  e.g. `"50000 100000"` for half a CPU) and `--pids` (`pids.max`).
- Join the cgroup at clone time with `SysProcAttr.UseCgroupFD` and add
  `CLONE_NEWCGROUP`. Inside, `/sys/fs/cgroup` is a read-only cgroup2 mount
  rooted at the container's group.
- After exit, report OOM kills from `memory.events` and remove the group.

Done when: a memory hog is OOM-killed (`memory.events` counts it), a fork bomb
is stopped, and `--cpus 0.5` shows up as about 50% CPU in `top`.

## M4: networking (done)

- Bridge `mb0` at 10.88.0.1/16 and one veth pair per container. The container
  end is created directly inside the child's network namespace, as `eth0`,
  between Start and the config write.
- File-based IP assignment (`O_EXCL`), a default route, `ip_forward`, iptables
  MASQUERADE, and `/etc/resolv.conf` without loopback nameservers.
- `-p HOST:CONTAINER` with DNAT in PREROUTING and OUTPUT, plus a proxy on
  127.0.0.1 for loopback traffic, like docker-proxy.
- `--net bridge|none|host`.
- Netlink through vishvananda/netlink. The container's interfaces are
  configured through a netlink socket opened inside its namespace
  (`NewHandleAt`), so no thread has to switch namespaces by hand.

Done when: two containers ping each other, reach the internet, and
`-p 8080:80` serves from the host.

Not covered: UDP ports, IPv6, and reaching a container's published port from
another container through the host address. That last one needs hairpin NAT
or `br_netfilter`.

## M5: lifecycle (done)

- `run -d`, `ps`, `stop`, `rm`, `exec`, `logs`, `gc`, and `--rm`.
- A record per container, `/var/lib/microbox/containers/<id>/state.json`,
  moving from created to running to exited. Containers stay until `rm`, as
  in docker. A container whose supervisor was killed shows as dead.
- `run -d` hands the container to a shim: microbox in a new session, with
  the output going to a log file.
- `stop` sends SIGTERM, waits, then writes `1` to `cgroup.kill`. `rm` is safe to
  run twice. `gc` cleans up after crashes, finding port rules by an
  `microbox:<id>` comment.
- `exec` joins the PID namespace and forks `exec-init`, which joins the rest
  itself. The plan was a cgo constructor like runc's `nsexec.c`, so that the
  mount namespace is joined before the Go runtime starts threads. It turned
  out that `unshare(CLONE_FS)` on a locked thread is enough, because the
  kernel's actual condition is a thread that doesn't share its root and
  working directory. Two more things only showed up when trying it:
  joining the mount namespace before the fork made the exec look for glibc's
  loader in the image, and cloning into the cgroup fails from inside the
  container's cgroup namespace (`nsdelegate`).
- `--init`: microbox's init stays PID 1, forwards signals and reaps zombies.

Done when: a detached container survives the terminal closing, and after
`stop` and `rm` no mount, cgroup, veth or iptables rule is left.

Not covered: container names, `start` for a stopped container, attaching to
a detached one, a terminal for `exec` or `-d` (`-t`), and log rotation.

## M6: security (done)

- Docker's default 14 capabilities: everything else leaves the bounding set,
  and the inheritable and ambient sets are emptied. `no_new_privs`.
- A seccomp deny-list in hand-written BPF, x86-64 only. It also blocks
  `clone` with namespace flags and gives `clone3` ENOSYS, as Docker does.
- Docker's masked and read-only paths in `/proc` and `/sys`. The special
  mounts already had `nosuid,nodev` from M2.
- The processes microbox runs inside containers start from a sealed memfd
  copy of its binary (runc's fix for CVE-2019-5736).
- `--init`'s PID 1 is now a fresh exec of microbox after the restrictions,
  so all its threads have them, and it isn't dumpable. That exec needs a
  static binary, so `make build` sets `CGO_ENABLED=0`, and `--init` checks
  for it.
- `run -d` and `stop` raced with init: a signal could reach microbox's init
  before it had exec'd the command. The supervisor now waits on a pipe that
  closes with that exec (fixed in M5's PR).

Done when: `mount`, `reboot` and loading kernel modules fail inside the container.

Not covered:
- User namespaces (rootless containers, or root in the container mapped to
  an unprivileged user). The image's files belong to the host's root, so
  the container's root would need idmapped mounts or a chowned copy of each
  image, and `mknod` stops working, so `/dev` would have to be bind mounts.
- AppArmor or SELinux.
- Options to change the defaults (`--cap-add`, `--privileged`, a seccomp
  profile).
- A seccomp filter for other architectures: microbox refuses to start
  containers there.

## M7: images (stretch)

Pull OCI images from Docker Hub and stack their layers as overlay lowerdirs.

## Pitfalls

- Forgetting `MS_PRIVATE` on `/` leaks the container's mounts to the host.
- `pivot_root` returns EINVAL unless new_root is a mount point and the old root
  isn't shared.
- PID 1 ignores signals it has no handler for, and it has to reap zombies.
- cgroup v2: you can't enable controllers in `subtree_control` on a cgroup that
  has processes in it.
- `setns` from Go needs a locked OS thread, and for the mount and user
  namespaces it must happen before the runtime starts threads.
- Cleanup has to be safe to repeat, because crashes leave veths, mounts and
  cgroups behind.
- With cgroup2's `nsdelegate`, `CLONE_INTO_CGROUP` fails with ENOENT from
  inside the target's cgroup namespace.
- Exec'ing a dynamically linked binary after `pivot_root` or `setns` into a
  mount namespace looks for its loader in the image.
- Until init has exec'd the command, signals sent to PID 1 reach microbox.
- Capabilities, `no_new_privs` and seccomp filters belong to a thread. In Go,
  lock the thread before applying them and exec from it.
