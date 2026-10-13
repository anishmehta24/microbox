package e2e

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anishmehta24/microbox/internal/network"
)

var inetRE = regexp.MustCompile(`inet (10\.88\.\d+\.\d+)/16`)

func TestBridgeNetwork(t *testing.T) {
	out := mustRun(t, "--hostname", "web", "alpine", "sh", "-c",
		"ls /sys/class/net | tr '\\n' ' '; echo; ip -4 addr show eth0; ip route; tail -1 /etc/hosts")
	if !strings.HasPrefix(out, "eth0 lo \n") {
		t.Errorf("interfaces: want only eth0 and lo\n%s", out)
	}
	m := inetRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no 10.88.0.0/16 address on eth0:\n%s", out)
	}
	if !strings.Contains(out, "default via 10.88.0.1 dev eth0") {
		t.Errorf("no default route via the bridge:\n%s", out)
	}
	if !strings.HasSuffix(out, m[1]+"\tweb") {
		t.Errorf("/etc/hosts doesn't map web to %s:\n%s", m[1], out)
	}
}

// A waits for a TCP message, B pings A and sends it, both over mb0.
func TestContainersReachEachOther(t *testing.T) {
	if skipReason != "" {
		t.Skip(skipReason)
	}
	a := exec.Command(bin, "run", "alpine", "sh", "-c", "ip -4 addr show eth0; nc -l -p 7000")
	stdout, err := a.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	a.Stderr = os.Stderr
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(30*time.Second, func() { a.Process.Kill() })
	defer timer.Stop()

	sc := bufio.NewScanner(stdout)
	var ip string
	for ip == "" && sc.Scan() {
		if m := inetRE.FindStringSubmatch(sc.Text()); m != nil {
			ip = m[1]
		}
	}
	if ip == "" {
		t.Fatal("container A printed no address")
	}
	// Retry the send: A's nc may not be listening yet.
	mustRun(t, "alpine", "sh", "-c", fmt.Sprintf(
		"ping -c 1 -W 2 %[1]s >/dev/null && for i in 1 2 3 4 5 6 7 8 9 10; do echo bye | nc %[1]s 7000 && exit 0; sleep 0.2; done; exit 1", ip))
	var rest []string
	for sc.Scan() {
		rest = append(rest, sc.Text())
	}
	a.Wait()
	if !slices.Contains(rest, "bye") {
		t.Errorf("container A received %q, want bye", rest)
	}
}

// Through NAT to the internet, with DNS through the host's resolver.
func TestInternet(t *testing.T) {
	if c, err := net.DialTimeout("tcp", "example.com:80", 5*time.Second); err != nil {
		t.Skipf("the host itself is offline: %v", err)
	} else {
		c.Close()
	}
	if got := mustRun(t, "alpine", "sh", "-c", "wget -q -T 10 -O - http://example.com | grep -o '<title>.*</title>'"); got != "<title>Example Domain</title>" {
		t.Errorf("got %q", got)
	}
}

func TestPublishedPort(t *testing.T) {
	if skipReason != "" {
		t.Skip(skipReason)
	}
	const port = "18080"
	ctr := exec.Command(bin, "run", "-p", port+":80", "alpine", "sh", "-c",
		`for i in 1 2; do printf "HTTP/1.0 200 OK\r\nContent-Length: 6\r\n\r\nhello\n" | nc -l -p 80 -w 1 >/dev/null; done`)
	ctr.Stdout, ctr.Stderr = os.Stderr, os.Stderr
	if err := ctr.Start(); err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(30*time.Second, func() { ctr.Process.Kill() })
	defer timer.Stop()

	// 127.0.0.1 goes through microbox's proxy; the bridge address is one of
	// the host's own, so it goes through the OUTPUT DNAT rule.
	for _, addr := range []string{"127.0.0.1", network.Gateway.String()} {
		if got := httpGet(t, net.JoinHostPort(addr, port)); got != "hello\n" {
			t.Errorf("GET via %s = %q, want hello", addr, got)
		}
	}
	if err := ctr.Wait(); err != nil {
		t.Fatalf("container: %v", err)
	}

	rules, err := exec.Command("iptables", "-w", "-t", "nat", "-S").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rules), "--dport "+port) {
		t.Errorf("DNAT rules left behind:\n%s", rules)
	}
	if c, err := net.Dial("tcp", "127.0.0.1:"+port); err == nil {
		c.Close()
		t.Error("the proxy still listens after the container exited")
	}
}

// httpGet fetches http://addr/, retrying while the container starts up.
func httpGet(t *testing.T, addr string) string {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	var lastErr error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := client.Get("http://" + addr + "/")
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			return string(body)
		}
		lastErr = err
	}
	t.Fatalf("GET %s: %v", addr, lastErr)
	return ""
}

func TestPortAlreadyInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:18081")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	before := networkState(t)
	out, code := microbox(t, "-p", "18081:80", "alpine", "true")
	if code != 125 || !strings.Contains(out, "publish port 18081") {
		t.Errorf("exit %d, want 125 and a port error:\n%s", code, out)
	}
	if after := networkState(t); after != before {
		t.Errorf("leftovers after a failed start: %s, before %s", after, before)
	}
}

func TestNetNone(t *testing.T) {
	out := mustRun(t, "--net", "none", "alpine", "sh", "-c", "ls /sys/class/net; ping -c 1 -W 1 127.0.0.1 >/dev/null && echo lo-up")
	if out != "lo\nlo-up" {
		t.Errorf("got %q, want only a working lo", out)
	}
}

func TestNetHost(t *testing.T) {
	host, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	if got := mustRun(t, "--net", "host", "alpine", "readlink", "/proc/self/ns/net"); got != host {
		t.Errorf("net namespace %s, want the host's %s", got, host)
	}
	for _, args := range [][]string{
		{"--net", "host", "-p", "8080:80", "alpine", "true"},
		{"--net", "overlay", "alpine", "true"},
		{"-p", "80", "alpine", "true"},
	} {
		if out, code := microbox(t, args...); code != 2 {
			t.Errorf("microbox run %s: exit %d, want 2\n%s", strings.Join(args, " "), code, out)
		}
	}
}

func TestNoNetworkLeftovers(t *testing.T) {
	before := networkState(t)
	mustRun(t, "-p", "18082:80", "alpine", "true")
	if after := networkState(t); after != before {
		t.Errorf("host network state changed from %s to %s", before, after)
	}
}

// networkState summarizes what a container adds on the host: veth ends
// (mb<id>, not the mb0 bridge) and reserved IPs.
func networkState(t *testing.T) string {
	t.Helper()
	var veths, ips []string
	links, err := os.ReadDir("/sys/class/net")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range links {
		if strings.HasPrefix(l.Name(), "mb") && l.Name() != network.BridgeName {
			veths = append(veths, l.Name())
		}
	}
	files, err := os.ReadDir(filepath.Join(network.StateDir, "ips"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, f := range files {
		ips = append(ips, f.Name())
	}
	return fmt.Sprintf("veths %v, ips %v", veths, ips)
}
