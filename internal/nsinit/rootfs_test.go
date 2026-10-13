package nsinit

import (
	"syscall"
	"testing"
)

// The host's own device nodes carry the kernel's encoding of the same numbers.
func TestMkdevMatchesHostDevices(t *testing.T) {
	for _, d := range devices {
		var st syscall.Stat_t
		if err := syscall.Stat("/dev/"+d.name, &st); err != nil {
			t.Skipf("stat /dev/%s: %v", d.name, err)
		}
		if got := uint64(mkdev(d.major, d.minor)); got != uint64(st.Rdev) {
			t.Errorf("mkdev(%d, %d) = %#x, /dev/%s has %#x", d.major, d.minor, got, d.name, st.Rdev)
		}
	}
}
