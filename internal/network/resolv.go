package network

import (
	"bytes"
	"net"
	"strings"
)

// Docker's fallback when the host has no usable nameserver.
const fallbackNameservers = "nameserver 8.8.8.8\nnameserver 8.8.4.4\n"

// ResolvConf adapts the host's resolv.conf for a container with its own
// network namespace. A nameserver on a loopback address (systemd-resolved's
// 127.0.0.53, a local dnsmasq) means the host's loopback, which the container
// can't reach: its 127.0.0.1 is its own lo. Those lines are dropped, and if no
// nameserver is left the fallback servers are added.
func ResolvConf(host []byte) []byte {
	var out bytes.Buffer
	servers := 0
	for _, line := range strings.SplitAfter(string(host), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
			if ip := net.ParseIP(f[1]); ip != nil && ip.IsLoopback() {
				continue
			}
			servers++
		}
		out.WriteString(line)
	}
	if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
		out.WriteByte('\n')
	}
	if servers == 0 {
		out.WriteString(fallbackNameservers)
	}
	return out.Bytes()
}
