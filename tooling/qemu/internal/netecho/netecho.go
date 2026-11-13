// Package netecho is the service naru's QEMU tests run to see what crosses the network between two
// Nodes, in place of a Machine: a Machine's traffic is what mvmd's masquerade acts on, and no test
// image carries a guest that can send it. A sender gives a datagram the source address of a Machine
// and a receiver reports the source address it arrives with; a route watcher reports the kernel's
// routes through a gateway as they appear and go.
package netecho

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	routeTable   = "/proc/net/route"
	sendInterval = 300 * time.Millisecond
	watchEvery   = 200 * time.Millisecond
	payload      = "naru-netecho"
)

var errNotIPv4 = errors.New("not an IPv4 address")

// Options says what the service does. A service with nothing to do only waits.
type Options struct {
	// Listen is the UDP addresses to receive on, each "<address>:<port>".
	Listen []string
	// Send is the datagrams to send, each "<from>=<to>": <from> is an address, which becomes the
	// datagram's source, and <to> is "<address>:<port>".
	Send []string
	// Alias gives the loopback every address named in Listen and as a source in Send, so that the
	// kernel treats it as the Node's own. A Machine's addresses are not any Node's, and no Machine
	// exists here.
	Alias bool
	// Routes reports the routes through a gateway, as they appear and go.
	Routes bool
	// After is a Unix socket the service waits to accept a connection before it sends, if not
	// empty: mvmd's, which has its masquerade in place by then.
	After string
}

// Run starts what Options names, then blocks until ctx ends. It returns an error when an address does
// not parse or cannot be used.
func Run(ctx context.Context, opts Options, log *slog.Logger) error {
	log.Info("started")
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer wg.Wait()
	defer cancel()
	for i, l := range opts.Listen {
		addr, err := net.ResolveUDPAddr("udp", l)
		if err != nil {
			return err
		}
		if opts.Alias {
			if err := addLocal(fmt.Sprintf("lo:nl%d", i), addr.IP); err != nil {
				return err
			}
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return err
		}
		log.Info("listening", "addr", l)
		wg.Add(1)
		go func() { defer wg.Done(); receive(ctx, conn, log) }()
	}
	if len(opts.Send) > 0 && opts.After != "" {
		if err := waitForSocket(ctx, opts.After); err != nil {
			return err
		}
		log.Info("socket accepts connections", "path", opts.After)
	}
	aliased := map[string]bool{}
	for i, s := range opts.Send {
		from, to, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("send %q is not <from>=<to>", s)
		}
		src, err := netip.ParseAddr(from)
		if err != nil {
			return err
		}
		dst, err := net.ResolveUDPAddr("udp", to)
		if err != nil {
			return err
		}
		if opts.Alias && !aliased[from] {
			aliased[from] = true
			if err := addLocal(fmt.Sprintf("lo:ns%d", i), net.IP(src.AsSlice())); err != nil {
				return err
			}
		}
		conn, err := net.DialUDP("udp", &net.UDPAddr{IP: net.IP(src.AsSlice())}, dst)
		if err != nil {
			return err
		}
		log.Info("sending", "from", from, "to", to)
		wg.Add(1)
		go func() { defer wg.Done(); send(ctx, conn) }()
	}
	if opts.Routes {
		wg.Add(1)
		go func() { defer wg.Done(); watchRoutes(ctx, log) }()
	}
	log.Info("running")
	<-ctx.Done()
	return nil
}

// waitForSocket dials the Unix socket until a connection is accepted or ctx ends.
func waitForSocket(ctx context.Context, path string) error {
	var d net.Dialer
	for {
		conn, err := d.DialContext(ctx, "unix", path)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("%s did not accept a connection", path), err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// receive logs the source of each datagram, the first time that source is seen.
func receive(ctx context.Context, conn *net.UDPConn, log *slog.Logger) {
	go func() { <-ctx.Done(); _ = conn.Close() }()
	seen := map[string]bool{}
	buf := make([]byte, 64)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if string(buf[:n]) != payload {
			continue
		}
		key := src.IP.String()
		if !seen[key] {
			seen[key] = true
			log.Info("received", "from", key, "to", conn.LocalAddr().(*net.UDPAddr).IP.String())
		}
	}
}

// send writes a datagram every interval until ctx ends. A write the kernel refuses, for want of a
// route, is not an error: the test reads the absence on the receiver.
func send(ctx context.Context, conn *net.UDPConn) {
	defer func() { _ = conn.Close() }()
	t := time.NewTicker(sendInterval)
	defer t.Stop()
	for {
		_, _ = conn.Write([]byte(payload))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// watchRoutes logs "route added" and "route removed" for each route through a gateway.
func watchRoutes(ctx context.Context, log *slog.Logger) {
	known := map[string]bool{}
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	for {
		b, err := os.ReadFile(routeTable)
		if err != nil {
			log.Error("read the route table", "err", err)
			return
		}
		now := map[string]bool{}
		for _, r := range gatewayRoutes(string(b)) {
			now[r] = true
			if !known[r] {
				log.Info("route added", "route", r)
			}
		}
		for r := range known {
			if !now[r] {
				log.Info("route removed", "route", r)
			}
		}
		known = now
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// gatewayRoutes returns "<prefix> via <gateway>" for each route of a /proc/net/route table that
// goes through a gateway and is not the default route.
func gatewayRoutes(table string) []string {
	var out []string
	lines := strings.Split(table, "\n")
	for _, line := range lines[min(1, len(lines)):] {
		f := strings.Fields(line)
		if len(f) < 8 {
			continue
		}
		dst, gw, mask := hexAddr(f[1]), hexAddr(f[2]), hexAddr(f[7])
		var flags uint64
		if _, err := fmt.Sscanf(f[3], "%x", &flags); err != nil || flags&0x2 == 0 { // RTF_GATEWAY
			continue
		}
		if !dst.IsValid() || !gw.IsValid() || !mask.IsValid() || dst.IsUnspecified() {
			continue
		}
		ones := 0
		for _, c := range mask.As4() {
			for ; c != 0; c <<= 1 {
				ones += int(c >> 7)
			}
		}
		out = append(out, fmt.Sprintf("%s/%d via %s", dst, ones, gw))
	}
	return out
}

// hexAddr reads an address of /proc/net/route: eight hex digits of the host's byte order, which is
// little-endian on every architecture the image runs on.
func hexAddr(s string) netip.Addr {
	var v uint32
	if _, err := fmt.Sscanf(s, "%08X", &v); err != nil {
		return netip.Addr{}
	}
	return netip.AddrFrom4([4]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}
