//go:build !amd64

package nsinit

import (
	"fmt"
	"runtime"
)

// The seccomp filter is written against the x86-64 system call table. On
// other architectures microbox refuses to start containers rather than run
// them without it.
func installSeccomp() error {
	return fmt.Errorf("no seccomp filter for %s yet", runtime.GOARCH)
}
