// Command microbox is a small Linux container runtime, built to learn what
// happens underneath Docker.
package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/anishmehta24/microbox/internal/container"
	"github.com/anishmehta24/microbox/internal/nsinit"
)

const usage = `usage: microbox COMMAND [ARG...]

Commands:
  run [OPTIONS] IMAGE [COMMAND [ARG...]] create and start a container
  ps [-a]                                list running (-a: all) containers
  stop [-t SECONDS] CONTAINER...         SIGTERM, then SIGKILL after the timeout (default 10)
  rm [-f] CONTAINER...                   remove stopped containers (-f: kill running ones first)
  exec CONTAINER COMMAND [ARG...]        run another command in a running container
  logs [-f] CONTAINER                    print a detached container's output (-f: follow)
  pull IMAGE[:TAG]                       download an image from a registry (Docker Hub by default)
  images                                 list images
  rmi IMAGE...                           remove images (gc removes their layers)
  gc                                     clean up after killed microbox processes, and unused layers
  version                                print microbox's version

IMAGE is a pulled image such as alpine:3.20, a directory under /var/lib/microbox/images,
or a path to a root filesystem starting with / or ./
CONTAINER is an ID or a unique prefix of one.

Run options:
  -d                run in the background and print the container ID
  --rm              remove the container when it exits
  --init            run an init as PID 1 that forwards signals and reaps zombies
  -u, --user USER   run as USER, UID, USER:GROUP or UID:GID (default: the image's user)
  --hostname NAME   container hostname (default: the container ID)
  --memory SIZE     memory limit, e.g. 256m or 1g; no swap on top
  --cpus N          CPU limit, e.g. 0.5 for half of one CPU
  --pids N          maximum number of processes
  --net MODE        bridge (default): own IP on mb0 with NAT; none: loopback only;
                    host: share the host's network
  -p HOST:CONTAINER publish a container TCP port on the host (repeatable)
`

var commands = map[string]func([]string) int{
	"run":    container.Run,
	"ps":     container.Ps,
	"stop":   container.Stop,
	"rm":     container.Rm,
	"exec":   container.Exec,
	"logs":   container.Logs,
	"pull":   container.Pull,
	"images": container.Images,
	"rmi":    container.Rmi,
	"gc":     container.GC,
	// Hidden: the supervisor of a detached container.
	container.ShimCommand: container.Shim,
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch cmd := os.Args[1]; cmd {
	// Hidden: microbox re-executes itself with these inside a container. They
	// only return if setup fails, by exiting.
	case nsinit.Command:
		nsinit.Main()
	case nsinit.ExecCommand:
		nsinit.ExecMain()
	case nsinit.ReapCommand:
		nsinit.ReapMain()
	case "help", "-h", "--help":
		fmt.Print(usage)
	case "version", "--version":
		fmt.Println("microbox", version())
	default:
		run, ok := commands[cmd]
		if !ok {
			fmt.Fprintf(os.Stderr, "microbox: unknown command %q\n\n%s", cmd, usage)
			os.Exit(2)
		}
		os.Exit(run(os.Args[2:]))
	}
}

// version is the one Go records in the binary: the tag for a build of a
// tagged commit or for `go install ...@v0.1.0`, otherwise a pseudo-version
// made from the commit, or "(devel)" outside a git checkout.
func version() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return "(unknown)"
}
