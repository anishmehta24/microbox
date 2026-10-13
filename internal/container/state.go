package container

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anishmehta24/microbox/internal/cgroups"
	"github.com/anishmehta24/microbox/internal/network"
	"github.com/anishmehta24/microbox/internal/spec"
)

// Container is the record of one container, ContainersDir/<id>/state.json.
// Its supervisor, the microbox process that started the container and waits
// for it, writes it as the container moves from created to running to exited.
type Container struct {
	ID         string                `json:"id"`
	Image      string                `json:"image"` // as given to run
	Created    time.Time             `json:"created"`
	Config     spec.Config           `json:"config"`
	Limits     cgroups.Limits        `json:"limits"`
	Net        network.Mode          `json:"net"`
	Ports      []network.PortMapping `json:"ports,omitempty"`
	Detached   bool                  `json:"detached"`
	AutoRemove bool                  `json:"autoRemove"` // --rm

	Status     Status    `json:"status"`
	Supervisor Proc      `json:"supervisor"`
	Pid1       Proc      `json:"pid1"` // the container's PID 1, as the host sees it
	ExitCode   int       `json:"exitCode"`
	Finished   time.Time `json:"finished"`
}

// Status is where a container is in its lifecycle.
type Status string

const (
	Created Status = "created" // set up, PID 1 not started yet
	Running Status = "running"
	Exited  Status = "exited"
	// Dead is never stored. It is what a created or running container is
	// when its supervisor is gone, which only happens if it was killed:
	// nothing will record the exit or clean up. microbox gc does that.
	Dead Status = "dead"
)

// State is the status, checked against whether the supervisor still exists.
func (c *Container) State() Status {
	if c.Status != Exited && !c.Supervisor.alive() {
		return Dead
	}
	return c.Status
}

func containerDir(id string) string { return filepath.Join(ContainersDir, id) }
func statePath(id string) string    { return filepath.Join(containerDir(id), "state.json") }
func logPath(id string) string      { return filepath.Join(containerDir(id), "log") }

// save writes the record to a temporary file and renames it into place, so a
// reader sees either the old record or the new one, never half of one.
func (c *Container) save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath(c.ID) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(c.ID))
}

func load(id string) (*Container, error) {
	b, err := os.ReadFile(statePath(id))
	if err != nil {
		return nil, err
	}
	var c Container
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("container %s: %w", id, err)
	}
	return &c, nil
}

// list returns every container, oldest first.
func list() ([]*Container, error) {
	entries, err := os.ReadDir(ContainersDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs []*Container
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue // being removed
		}
		c, err := load(e.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue // created or removed meanwhile
		}
		if err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	slices.SortFunc(cs, func(a, b *Container) int { return a.Created.Compare(b.Created) })
	return cs, nil
}

// find looks a container up by its ID or a unique prefix of it, like docker.
func find(ref string) (*Container, error) {
	cs, err := list()
	if err != nil {
		return nil, err
	}
	var match []*Container
	for _, c := range cs {
		if c.ID == ref {
			return c, nil
		}
		if ref != "" && strings.HasPrefix(c.ID, ref) {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("no such container: %s", ref)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("%s matches %d containers, give more of the ID", ref, len(match))
}

// remove deletes a container's directory: the record, the writable layer and
// the log. It renames the directory first, which makes the container
// disappear from ps at once, and lets gc finish a removal that was cut short.
func remove(id string) error {
	trash := filepath.Join(ContainersDir, ".removing-"+id)
	if err := os.Rename(containerDir(id), trash); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.RemoveAll(trash)
}

// Proc identifies a process on the host. PIDs get reused, so it records the
// process's start time too: a PID with a different start time is another
// process.
type Proc struct {
	Pid   int    `json:"pid"`
	Start uint64 `json:"start"` // clock ticks after boot
}

func procOf(pid int) (Proc, error) {
	start, _, err := procStat(pid)
	return Proc{pid, start}, err
}

// alive reports whether the process still exists. A zombie doesn't count: it
// has exited and only waits for its parent to collect the status.
func (p Proc) alive() bool {
	if p.Pid <= 0 {
		return false
	}
	start, state, err := procStat(p.Pid)
	return err == nil && start == p.Start && state != 'Z' && state != 'X'
}

// procStat reads the start time and state from /proc/<pid>/stat:
// "pid (comm) state ppid ... starttime ...", where starttime is field 22.
func procStat(pid int) (start uint64, state byte, err error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, err
	}
	// comm can contain spaces and parentheses; the last ')' ends it.
	fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
	if len(fields) < 20 {
		return 0, 0, fmt.Errorf("/proc/%d/stat: too few fields", pid)
	}
	start, err = strconv.ParseUint(fields[19], 10, 64) // field 22, counting from pid
	return start, fields[0][0], err
}
