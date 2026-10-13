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

## M5: lifecycle

- `run -d`, `ps`, `stop`, `rm`, `exec`, `logs`.
- State in `/var/lib/microbox/containers/<id>/state.json`, moving created →
  running → stopped.
- `stop` sends SIGTERM, waits, then writes `1` to `cgroup.kill`. `rm` is safe to
  run twice. `gc` cleans up after crashes.
- `exec` uses `setns` on `/proc/<pid>/ns/*`. Joining a mount namespace has to
  happen before the Go runtime starts threads, so it needs a cgo constructor
  (runc's `nsexec.c` approach).
- `--init`: a small PID 1 that forwards signals and reaps zombies.

Done when: a detached container survives the terminal closing, and after
`stop` and `rm` no mount, cgroup, veth or iptables rule is left.

## M6: security

- Drop to Docker's default capability set, and set `no_new_privs`.
- A seccomp deny-list.
- Mask `/proc/kcore`, `/proc/sys` and similar. Mount with `nosuid,nodev`.
- Optional user namespace (rootless).

Done when: `mount`, `reboot` and loading kernel modules fail inside the container.

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
