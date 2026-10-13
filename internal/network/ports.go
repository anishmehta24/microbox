package network

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
)

// PortMapping publishes a container's TCP port on a host port.
type PortMapping struct {
	Host, Container int
}

// ParsePort reads HOST:CONTAINER, as in -p 8080:80.
func ParsePort(s string) (PortMapping, error) {
	h, c, ok := strings.Cut(s, ":")
	host, herr := strconv.Atoi(h)
	ctr, cerr := strconv.Atoi(c)
	if !ok || herr != nil || cerr != nil || host < 1 || host > 65535 || ctr < 1 || ctr > 65535 {
		return PortMapping{}, fmt.Errorf("invalid port mapping %q: want HOST:CONTAINER, e.g. 8080:80 (TCP only)", s)
	}
	return PortMapping{Host: host, Container: ctr}, nil
}

func (p PortMapping) String() string { return fmt.Sprintf("%d:%d", p.Host, p.Container) }

// publish forwards host port p.Host to the container, in two ways like Docker:
//
//   - DNAT rules rewrite the destination of packets for the host port to the
//     container's address. PREROUTING catches packets arriving from outside,
//     OUTPUT catches connections the host makes to one of its own addresses.
//   - 127.0.0.1 can't be DNATed this way, because the kernel won't route a
//     packet from a loopback address out of another interface. A small proxy
//     in microbox listens there instead, like docker-proxy. Holding that port
//     also stops a second container from publishing the same one.
func (e *Endpoint) publish(p PortMapping) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.Host))
	if err != nil {
		return fmt.Errorf("publish port %d: %w", p.Host, err)
	}
	e.proxies = append(e.proxies, ln)
	target := net.JoinHostPort(e.IP.String(), strconv.Itoa(p.Container))
	go proxy(ln, target)

	dnat := []string{"-p", "tcp", "--dport", strconv.Itoa(p.Host),
		"-m", "addrtype", "--dst-type", "LOCAL", "-j", "DNAT", "--to-destination", target}
	for _, r := range []rule{
		{"nat", "PREROUTING", dnat},
		{"nat", "OUTPUT", append([]string{"!", "-d", "127.0.0.0/8"}, dnat...)},
	} {
		if err := r.add(); err != nil {
			return err
		}
		e.rules = append(e.rules, r)
	}
	return nil
}

// proxy accepts connections until ln is closed and relays each to target.
func proxy(ln net.Listener, target string) {
	for {
		client, err := ln.Accept()
		if err != nil {
			return
		}
		go relay(client.(*net.TCPConn), target)
	}
}

// relay copies both directions. When one side finishes sending, it passes the
// half-close on (CloseWrite) and keeps the other direction open, like TCP itself.
func relay(client *net.TCPConn, target string) {
	defer client.Close()
	c, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	server := c.(*net.TCPConn)
	defer server.Close()
	done := make(chan struct{})
	go func() {
		io.Copy(server, client)
		server.CloseWrite()
		close(done)
	}()
	io.Copy(client, server)
	client.CloseWrite()
	<-done
}

// rule is one iptables rule: a table, a chain and the match and target.
type rule struct {
	table, chain string
	spec         []string
}

func (r rule) add() error    { return iptables(r.table, "-A", r.chain, r.spec) }
func (r rule) delete() error { return iptables(r.table, "-D", r.chain, r.spec) }

// ensure adds the rule unless an identical one exists. insert puts it at the
// top of the chain instead of the bottom.
func (r rule) ensure(insert bool) error {
	if iptables(r.table, "-C", r.chain, r.spec) == nil {
		return nil
	}
	if insert {
		return iptables(r.table, "-I", r.chain, r.spec)
	}
	return r.add()
}

// iptables runs one iptables command. -w waits for the xtables lock instead of
// failing while another program changes rules at the same time.
func iptables(table, op, chain string, spec []string) error {
	args := append([]string{"-w", "-t", table, op, chain}, spec...)
	out, err := exec.Command("iptables", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s: %v: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}
