package nsinit

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/anishmehta24/microbox/internal/spec"
)

// enter moves the process to the command's working directory and user. It
// runs after restrict, which needs root: removing capabilities from the
// bounding set takes CAP_SETPCAP. Switching to another user then also empties
// the process's capabilities, as in docker.
func enter(p *spec.Process) error {
	cwd := p.Cwd
	if cwd == "" {
		cwd = "/"
	}
	// Like docker, create a working directory the image doesn't have.
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	id, err := lookupUser(p.User)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(p.Env, func(kv string) bool { return strings.HasPrefix(kv, "HOME=") }) {
		p.Env = append(p.Env, "HOME="+id.home)
	}
	// Go applies these to every thread of the process.
	if err := syscall.Setgroups(id.groups); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(id.gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(id.uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	return nil
}

// identity is who the command runs as.
type identity struct {
	uid, gid int
	groups   []int // supplementary groups
	home     string
}

// lookupUser reads a user the way docker reads USER and --user: NAME, UID,
// NAME:GROUP or UID:GID, with names looked up in the container's /etc/passwd
// and /etc/group. A UID that /etc/passwd doesn't list still works, with GID 0
// and / as its home. Empty means root.
func lookupUser(s string) (identity, error) {
	user, group, hasGroup := strings.Cut(s, ":")
	if user == "" {
		user = "0"
	}
	passwd, err := readEntries("/etc/passwd", 7)
	if err != nil {
		return identity{}, err
	}
	id := identity{home: "/"}
	var name string
	// name:password:uid:gid:gecos:home:shell
	entry := slices.IndexFunc(passwd, func(e []string) bool { return e[0] == user || e[2] == user })
	if uid, err := strconv.Atoi(user); err == nil {
		id.uid = uid
	} else if entry < 0 {
		return identity{}, fmt.Errorf("user %q isn't in the image's /etc/passwd", user)
	}
	if entry >= 0 {
		e := passwd[entry]
		name, id.home = e[0], e[5]
		if id.uid, err = strconv.Atoi(e[2]); err != nil {
			return identity{}, fmt.Errorf("/etc/passwd: bad uid for %s", name)
		}
		if id.gid, err = strconv.Atoi(e[3]); err != nil {
			return identity{}, fmt.Errorf("/etc/passwd: bad gid for %s", name)
		}
	}

	groups, err := readEntries("/etc/group", 4)
	if err != nil {
		return identity{}, err
	}
	// name:password:gid:member,member,...
	if hasGroup {
		g := slices.IndexFunc(groups, func(e []string) bool { return e[0] == group || e[2] == group })
		if gid, err := strconv.Atoi(group); err == nil {
			id.gid = gid
		} else if g < 0 {
			return identity{}, fmt.Errorf("group %q isn't in the image's /etc/group", group)
		} else if id.gid, err = strconv.Atoi(groups[g][2]); err != nil {
			return identity{}, fmt.Errorf("/etc/group: bad gid for %s", group)
		}
	}
	id.groups = []int{id.gid}
	for _, e := range groups {
		if name == "" || !slices.Contains(strings.Split(e[3], ","), name) {
			continue
		}
		if gid, err := strconv.Atoi(e[2]); err == nil && !slices.Contains(id.groups, gid) {
			id.groups = append(id.groups, gid)
		}
	}
	return id, nil
}

// readEntries reads a colon-separated file like /etc/passwd, keeping the
// lines with at least n fields. A missing file has no entries.
func readEntries(path string, n int) ([][]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fields := strings.Split(line, ":"); len(fields) >= n {
			entries = append(entries, fields)
		}
	}
	return entries, sc.Err()
}
