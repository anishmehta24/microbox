package network

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
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

func (a *ipam) release(ip net.IP) error {
	err := os.Remove(filepath.Join(a.dir, ip.String()))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
