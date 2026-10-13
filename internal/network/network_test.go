package network

import (
	"fmt"
	"net"
	"testing"
)

func TestParsePort(t *testing.T) {
	if got, err := ParsePort("8080:80"); err != nil || got != (PortMapping{8080, 80}) {
		t.Errorf("ParsePort(8080:80) = %v, %v", got, err)
	}
	for _, in := range []string{"", "80", "8080:", ":80", "0:80", "8080:65536", "a:b", "8080:80/udp", "1:2:3"} {
		if _, err := ParsePort(in); err == nil {
			t.Errorf("ParsePort(%q) succeeded, want an error", in)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, m := range []string{"bridge", "none", "host"} {
		if got, err := ParseMode(m); err != nil || string(got) != m {
			t.Errorf("ParseMode(%q) = %q, %v", m, got, err)
		}
	}
	if _, err := ParseMode("overlay"); err == nil {
		t.Error("ParseMode(overlay) succeeded")
	}
}

func TestResolvConf(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"keeps a reachable server", "nameserver 10.255.255.254\n", "nameserver 10.255.255.254\n"},
		{"drops loopback, keeps the rest",
			"# generated\nnameserver 127.0.0.53\nnameserver 192.168.1.1\nsearch lan\n",
			"# generated\nnameserver 192.168.1.1\nsearch lan\n"},
		{"falls back when only loopback is left",
			"nameserver 127.0.0.53\nnameserver ::1\noptions edns0\n",
			"options edns0\n" + fallbackNameservers},
		{"falls back on an empty file", "", fallbackNameservers},
		{"adds the missing final newline", "nameserver 1.1.1.1", "nameserver 1.1.1.1\n"},
	} {
		if got := string(ResolvConf([]byte(tc.in))); got != tc.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestIPAM(t *testing.T) {
	// A /29 has 8 addresses: network, gateway, 5 for containers, broadcast.
	a := &ipam{dir: t.TempDir(), subnet: &net.IPNet{IP: net.IPv4(10, 0, 0, 0).To4(), Mask: net.CIDRMask(29, 32)}}
	var got []string
	for i := 0; i < 5; i++ {
		ip, err := a.allocate("c")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ip.String())
	}
	if want := "[10.0.0.2 10.0.0.3 10.0.0.4 10.0.0.5 10.0.0.6]"; fmt.Sprint(got) != want {
		t.Errorf("allocated %v, want %s", got, want)
	}
	if _, err := a.allocate("c"); err == nil {
		t.Error("allocated a 6th address in a /29")
	}
	if err := a.release(net.ParseIP("10.0.0.4")); err != nil {
		t.Fatal(err)
	}
	if ip, err := a.allocate("c"); err != nil || ip.String() != "10.0.0.4" {
		t.Errorf("after release got %v, %v; want 10.0.0.4 again", ip, err)
	}
	if err := a.release(net.ParseIP("10.0.0.99")); err != nil {
		t.Errorf("releasing an unknown address: %v", err)
	}
}
