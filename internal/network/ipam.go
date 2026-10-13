package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// ipam hands out addresses in subnet. Each address in use is a file in dir
// named after it, holding the container ID.
type ipam struct {
	dir    string
	subnet *net.IPNet
}

// allocate reserves the first free address. Creating the file with O_EXCL is
// atomic, so two containers starting at the same time can't both get one.
func (a *ipam) allocate(id string) (net.IP, error) {
	if err := os.MkdirAll(a.dir, 0o755); err != nil {
		return nil, err
	}
	base := binary.BigEndian.Uint32(a.subnet.IP.To4())
	ones, bits := a.subnet.Mask.Size()
	size := uint32(1) << (bits - ones)
	// Skip the network address, the gateway (.1) and the broadcast address.
	for i := uint32(2); i < size-1; i++ {
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, base+i)
		f, err := os.OpenFile(filepath.Join(a.dir, ip.String()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString(id + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return ip, err
	}
	return nil, fmt.Errorf("no free addresses left in %s", a.subnet)
}

// release frees the addresses held by container id.
func (a *ipam) release(id string) error {
	held, err := a.read()
	if err != nil {
		return err
	}
	for ip, owner := range held {
		if owner != id {
			continue
		}
		if err := os.Remove(filepath.Join(a.dir, ip)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// owners returns the IDs of the containers holding an address.
func (a *ipam) owners() (map[string]bool, error) {
	held, err := a.read()
	if err != nil {
		return nil, err
	}
	owners := map[string]bool{}
	for _, id := range held {
		owners[id] = true
	}
	return owners, nil
}

// read maps each reserved address to the container holding it.
func (a *ipam) read() (map[string]string, error) {
	entries, err := os.ReadDir(a.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	held := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(a.dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue // released meanwhile
		}
		if err != nil {
			return nil, err
		}
		// Empty for a moment while allocate writes it.
		if id := strings.TrimSpace(string(b)); id != "" {
			held[e.Name()] = id
		}
	}
	return held, nil
}
