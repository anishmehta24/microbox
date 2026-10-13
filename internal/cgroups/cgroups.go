// Package cgroups gives each container its own cgroup v2 group, with optional
// memory, CPU and process-count limits.
package cgroups

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	Root   = "/sys/fs/cgroup"   // the cgroup2 mount
	Parent = Root + "/microbox" // holds one group per container, never processes
)

// cpuPeriod is the cpu.max period in microseconds, the kernel's default.
const cpuPeriod = 100000

// Limits for a container. Zero means unlimited.
type Limits struct {
	MemoryBytes int64
	CPUs        float64 // 0.5 means half of one CPU
	Pids        int64
}

// Cgroup is one container's group, Parent/<id>.
type Cgroup struct {
	Path string
}

// Create makes the container's group and writes its limits.
func Create(id string, l Limits) (*Cgroup, error) {
	return create(Root, id, l)
}

func create(root, id string, l Limits) (*Cgroup, error) {
	enabled, err := setupParent(root)
	if err != nil {
		return nil, err
	}
	files := limitFiles(l)
	for _, f := range files {
		if controller, _, _ := strings.Cut(f.name, "."); !enabled[controller] {
			return nil, fmt.Errorf("can't set %s: this host's cgroup v2 doesn't offer the %s controller", f.name, controller)
		}
	}
	cg := &Cgroup{Path: filepath.Join(root, "microbox", id)}
	if err := os.Mkdir(cg.Path, 0o755); err != nil {
		return nil, err
	}
	for _, f := range files {
		err := os.WriteFile(filepath.Join(cg.Path, f.name), []byte(f.value), 0o644)
		// memory.swap.max only exists when the kernel accounts swap, and then
		// there is no swap to turn off.
		if errors.Is(err, fs.ErrNotExist) && f.name == "memory.swap.max" {
			continue
		}
		if err != nil {
			cg.Remove()
			return nil, fmt.Errorf("set %s: %w", f.name, err)
		}
	}
	return cg, nil
}

// wanted are the controllers the limits use.
var wanted = []string{"cpu", "memory", "pids"}

// setupParent creates root/microbox and enables controllers for its children.
// A controller is only available in a group if every ancestor lists it in
// cgroup.subtree_control. The parent never holds processes itself: cgroup v2's
// "no internal processes" rule forbids handing controllers down from a group
// that has processes of its own.
//
// Only the wanted controllers that root offers are enabled. A host can keep
// some controllers on cgroup v1 or disable them, and a subtree_control write
// naming any unavailable controller fails as a whole. It returns the enabled
// set, so only a limit that needs a missing controller fails.
func setupParent(root string) (map[string]bool, error) {
	offered, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, fmt.Errorf("cgroup v2 is not mounted at %s: %w", root, err)
	}
	enabled := map[string]bool{}
	var enable []string
	for _, c := range wanted {
		if slices.Contains(strings.Fields(string(offered)), c) {
			enabled[c] = true
			enable = append(enable, "+"+c)
		}
	}
	parent := filepath.Join(root, "microbox")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	if len(enable) == 0 {
		return enabled, nil
	}
	for _, dir := range []string{root, parent} {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(strings.Join(enable, " ")), 0o644); err != nil {
			return nil, fmt.Errorf("enable controllers in %s: %w", dir, err)
		}
	}
	return enabled, nil
}

type limitFile struct{ name, value string }

func limitFiles(l Limits) []limitFile {
	var files []limitFile
	if l.MemoryBytes > 0 {
		files = append(files,
			limitFile{"memory.max", strconv.FormatInt(l.MemoryBytes, 10)},
			// Otherwise the container swaps out instead of hitting the limit.
			limitFile{"memory.swap.max", "0"})
	}
	if l.CPUs > 0 {
		// "quota period": the group may run quota µs of CPU time per period µs,
		// summed over all CPUs. 0.5 CPUs is 50000 µs every 100000 µs.
		quota := int64(l.CPUs*cpuPeriod + 0.5)
		files = append(files, limitFile{"cpu.max", fmt.Sprintf("%d %d", quota, cpuPeriod)})
	}
	if l.Pids > 0 {
		files = append(files, limitFile{"pids.max", strconv.FormatInt(l.Pids, 10)})
	}
	return files
}

// OOMKills reports how many processes the kernel's OOM killer killed in the
// group because it reached memory.max.
func (c *Cgroup) OOMKills() (int64, error) {
	f, err := os.Open(filepath.Join(c.Path, "memory.events"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "oom_kill "); ok {
			return strconv.ParseInt(v, 10, 64)
		}
	}
	return 0, sc.Err()
}

// Lookup returns the group of container id, whether or not it exists.
func Lookup(id string) *Cgroup {
	return &Cgroup{Path: filepath.Join(Parent, id)}
}

// Remove deletes the group. The kernel only allows that once it has no
// processes left, which is the case after the container's PID 1 is reaped:
// the PID namespace dies with it.
func (c *Cgroup) Remove() error {
	return os.Remove(c.Path)
}

// Kill sends SIGKILL to every process in the group at once, through
// cgroup.kill (Linux 5.14+). Processes that microbox exec started are in the
// group too, and so is anything they forked, even mid-fork.
func (c *Cgroup) Kill() error {
	f, err := os.OpenFile(filepath.Join(c.Path, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("1")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Destroy kills whatever is left in the group and removes it. It cleans up
// after a supervisor that died: normally the group is already empty when it
// is removed. A group that doesn't exist is not an error.
func (c *Cgroup) Destroy() error {
	if err := c.Kill(); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	// The kill is asynchronous, and rmdir fails with EBUSY until the last
	// process is gone.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Remove(c.Path)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ParseBytes reads a size like 512k, 256m or 1g (powers of 1024) or a plain
// number of bytes.
func ParseBytes(s string) (int64, error) {
	num, mult := strings.ToLower(s), int64(1)
	if n := len(num); n > 0 {
		switch num[n-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		}
		if mult > 1 {
			num = num[:n-1]
		}
	}
	v, err := strconv.ParseInt(num, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q: want a positive number with an optional k, m or g suffix", s)
	}
	if v > (1<<63-1)/mult {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return v * mult, nil
}
