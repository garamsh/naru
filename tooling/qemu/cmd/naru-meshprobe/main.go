// Command naru-meshprobe is the probe of the mesh test (#385): it waits for the WireGuard interface
// it is given to hold an IPv4 address, the host's mesh address, answers TCP on that address and the
// port with "naru94-mesh-answer <mesh address>", and every two seconds connects from it to each
// other address it is given and prints
//
//	naru94-mesh: self=<mesh address> <address>=<the answer's address, or fail>... rx=<bytes> tx=<bytes>
//
// where rx and tx are the interface's counters (docs/architecture/harness.md).
package main

import (
	"bufio"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: naru-meshprobe <interface> <port> <address>...")
		os.Exit(2)
	}
	iface, port, targets := os.Args[1], os.Args[2], os.Args[3:]
	self := meshAddress(iface)
	l, err := net.Listen("tcp", net.JoinHostPort(self.String(), port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "naru-meshprobe:", err)
		os.Exit(1)
	}
	go serve(l, self)
	for ; ; time.Sleep(2 * time.Second) {
		parts := []string{"self=" + self.String()}
		for _, t := range targets {
			if t != self.String() {
				parts = append(parts, t+"="+ask(self, net.JoinHostPort(t, port)))
			}
		}
		parts = append(parts, "rx="+counter(iface, "rx_bytes"), "tx="+counter(iface, "tx_bytes"))
		fmt.Printf("naru94-mesh: %s\n", strings.Join(parts, " "))
	}
}

// meshAddress waits for the interface to exist and hold an IPv4 address, and returns it.
func meshAddress(name string) netip.Addr {
	for ; ; time.Sleep(time.Second) {
		i, err := net.InterfaceByName(name)
		if err != nil {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap().Is4() {
					return ip.Unmap()
				}
			}
		}
	}
}

func serve(l net.Listener, self netip.Addr) {
	for {
		c, err := l.Accept()
		if err != nil {
			continue
		}
		_, _ = fmt.Fprintf(c, "naru94-mesh-answer %s\n", self)
		_ = c.Close()
	}
}

// ask connects from self to target and returns the address the answer names, or "fail".
func ask(self netip.Addr, target string) string {
	d := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: self.AsSlice()}}
	c, err := d.Dial("tcp", target)
	if err != nil {
		return "fail"
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "fail"
	}
	addr, ok := strings.CutPrefix(strings.TrimSpace(line), "naru94-mesh-answer ")
	if !ok {
		return "fail"
	}
	return addr
}

// counter is the interface's statistic, or "0" when it cannot be read.
func counter(iface, name string) string {
	b, err := os.ReadFile("/sys/class/net/" + iface + "/statistics/" + name)
	if err != nil {
		return "0"
	}
	return strings.TrimSpace(string(b))
}
