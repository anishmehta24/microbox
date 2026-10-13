// Package network connects containers to the host the way Docker's default
// bridge network does: a bridge on the host, one veth pair per container,
// NAT for outgoing traffic and DNAT for published ports.
package network

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	BridgeName = "mb0"
	StateDir   = "/var/lib/microbox/network"
)

var (
	Subnet  = &net.IPNet{IP: net.IPv4(10, 88, 0, 0).To4(), Mask: net.CIDRMask(16, 32)}
	Gateway = net.IPv4(10, 88, 0, 1).To4() // the bridge's address on the host
)

// Mode is how a container is networked.
type Mode string

const (
	Bridge Mode = "bridge" // own network namespace with a veth to mb0 (the default)
	None   Mode = "none"   // own network namespace with only loopback
	Host   Mode = "host"   // the host's network namespace: no isolation
)

func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Bridge, None, Host:
		return m, nil
	}
	return "", fmt.Errorf("unknown network %q: want bridge, none or host", s)
}

// Endpoint is one container's network. Prepare and Attach build it up,
// Teardown takes it apart.
type Endpoint struct {
	Mode Mode
	IP   net.IP // only in bridge mode

	id      string
	proxies []net.Listener
}

// addrs hands out the container addresses.
var addrs = &ipam{dir: filepath.Join(StateDir, "ips"), subnet: Subnet}

// hostVeth names the host end of a container's veth pair. Interface names are
// limited to 15 characters, which "mb" and a 12-character ID fit.
func hostVeth(id string) string { return "mb" + id }

// Prepare sets up everything that doesn't need the container process yet:
// the shared bridge and NAT, an IP address, and the published ports. Doing the
// ports first means a port that is already taken fails before anything starts.
func Prepare(id string, mode Mode, ports []PortMapping) (*Endpoint, error) {
	e := &Endpoint{Mode: mode, id: id}
	if len(ports) > 0 && mode != Bridge {
		return nil, errors.New("publishing ports needs bridge networking")
	}
	if mode != Bridge {
		return e, nil
	}
	if err := ensureHostNetwork(); err != nil {
		return nil, err
	}
	ip, err := addrs.allocate(id)
	if err != nil {
		return nil, err
	}
	e.IP = ip
	for _, p := range ports {
		if err := e.publish(p); err != nil {
			e.Teardown()
			return nil, err
		}
	}
	return e, nil
}

// Attach wires the network into the container. It runs after the container
// process exists (its network namespace is created with it) but before init
// is allowed to continue, so the command starts with the network ready.
func (e *Endpoint) Attach(pid int) error {
	if e.Mode == Host {
		return nil
	}
	if e.Mode == Bridge {
		br, err := netlink.LinkByName(BridgeName)
		if err != nil {
			return err
		}
		// ip link add mb<id> master mb0 type veth peer name eth0 netns <pid>
		// A veth pair is a virtual cable. One end stays on the host, plugged
		// into the bridge; the other is created inside the container.
		veth := &netlink.Veth{
			LinkAttrs:     netlink.LinkAttrs{Name: hostVeth(e.id), MasterIndex: br.Attrs().Index},
			PeerName:      "eth0",
			PeerNamespace: netlink.NsPid(pid),
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("create veth pair: %w", err)
		}
		if err := netlink.LinkSetUp(veth); err != nil {
			return err
		}
	}
	return e.configureInside(pid)
}

// configureInside sets up the interfaces in the container's namespace. The
// netlink handle's socket is opened inside that namespace, so the calls act on
// the container's interfaces, the same as `nsenter -t <pid> -n ip ...`.
func (e *Endpoint) configureInside(pid int) error {
	ns, err := netns.GetFromPid(pid)
	if err != nil {
		return err
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return err
	}
	defer h.Close()

	// A new network namespace starts with only lo, and lo is down.
	lo, err := h.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := h.LinkSetUp(lo); err != nil {
		return err
	}
	if e.Mode != Bridge {
		return nil
	}
	eth0, err := h.LinkByName("eth0")
	if err != nil {
		return err
	}
	// ip addr add 10.88.x.y/16 dev eth0
	if err := h.AddrAdd(eth0, &netlink.Addr{IPNet: &net.IPNet{IP: e.IP, Mask: Subnet.Mask}}); err != nil {
		return fmt.Errorf("set container address: %w", err)
	}
	if err := h.LinkSetUp(eth0); err != nil {
		return err
	}
	// ip route add default via 10.88.0.1
	if err := h.RouteAdd(&netlink.Route{LinkIndex: eth0.Attrs().Index, Gw: Gateway}); err != nil {
		return fmt.Errorf("add default route: %w", err)
	}
	return nil
}

// Teardown stops the port proxies and then removes what Cleanup removes. It is
// safe on a partly built endpoint. The container side needs no cleanup: its
// network namespace, eth0 included, disappears with its last process.
func (e *Endpoint) Teardown() error {
	for _, ln := range e.proxies {
		ln.Close()
	}
	if e.Mode != Bridge {
		return nil
	}
	return Cleanup(e.id)
}

// Cleanup removes what bridge networking set up on the host for container id:
// its port rules, its veth and its address. It finds them by the ID alone, so
// it still works after the microbox process that set them up was killed
// (microbox gc), and running it twice is harmless.
func Cleanup(id string) error {
	var errs []error
	rules, err := listRules()
	errs = append(errs, err)
	for _, r := range rules[id] {
		errs = append(errs, r.delete())
	}
	// Deleting either end of a veth deletes both. The kernel does that by
	// itself once the container's namespace is gone, but asynchronously.
	if l, err := netlink.LinkByName(hostVeth(id)); err == nil {
		if err := netlink.LinkDel(l); err != nil && !errors.Is(err, syscall.ENODEV) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, addrs.release(id))
	return errors.Join(errs...)
}

// Owners returns the IDs of the containers that hold an address, a veth or
// port rules on the host.
func Owners() (map[string]bool, error) {
	owners, err := addrs.owners()
	if err != nil {
		return nil, err
	}
	rules, err := listRules()
	if err != nil {
		return nil, err
	}
	for id := range rules {
		owners[id] = true
	}
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	for _, l := range links {
		if id, ok := strings.CutPrefix(l.Attrs().Name, "mb"); ok && l.Type() == "veth" {
			owners[id] = true
		}
	}
	return owners, nil
}

// ensureHostNetwork sets up what all containers share: the mb0 bridge holding
// the gateway address, IP forwarding, and the NAT and forwarding rules. Every
// step is idempotent, so each run simply makes sure.
func ensureHostNetwork() error {
	// ip link add mb0 type bridge
	err := netlink.LinkAdd(&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: BridgeName}})
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("create bridge: %w", err)
	}
	br, err := netlink.LinkByName(BridgeName)
	if err != nil {
		return err
	}
	// ip addr replace 10.88.0.1/16 dev mb0; ip link set mb0 up
	if err := netlink.AddrReplace(br, &netlink.Addr{IPNet: &net.IPNet{IP: Gateway, Mask: Subnet.Mask}}); err != nil {
		return fmt.Errorf("set bridge address: %w", err)
	}
	if err := netlink.LinkSetUp(br); err != nil {
		return err
	}
	// Without forwarding the host drops packets that aren't addressed to it,
	// so nothing would get from mb0 to the outside world.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	for _, r := range []struct {
		rule
		insert bool
	}{
		// Container traffic leaving the host goes out with the host's address.
		// Not traffic between containers, which stays on mb0.
		{rule{"nat", "POSTROUTING", []string{"-s", Subnet.String(), "!", "-o", BridgeName, "-j", "MASQUERADE"}}, false},
		// Let bridge traffic through FORWARD even if its policy is DROP, which
		// it is wherever Docker is installed. Inserted ahead of other rules.
		{rule{"filter", "FORWARD", []string{"-i", BridgeName, "-j", "ACCEPT"}}, true},
		{rule{"filter", "FORWARD", []string{"-o", BridgeName, "-j", "ACCEPT"}}, true},
	} {
		if err := r.ensure(r.insert); err != nil {
			return err
		}
	}
	return nil
}
