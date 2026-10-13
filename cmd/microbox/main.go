// Command microbox is a small Linux container runtime, built to learn what
// happens underneath Docker.
package main

import (
	"fmt"
	"os"

	"github.com/anishmehta24/microbox/internal/container"
	"github.com/anishmehta24/microbox/internal/nsinit"
)

const usage = `usage: microbox run [--hostname NAME] IMAGE COMMAND [ARG...]

IMAGE is a directory under /var/lib/microbox/images, or a path to a root filesystem.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		os.Exit(container.Run(os.Args[2:]))
	case nsinit.Command:
		// Hidden: microbox re-executes itself with this argument inside the new
		// namespaces. Main only returns if setup fails.
		nsinit.Main()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "microbox: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
