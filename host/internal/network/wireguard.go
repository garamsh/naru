package network

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// routeProtocol marks the routes the host makes for the tunnel's peers, so a reconfiguration
// removes those and only those.
const routeProtocol = 190

// WireGuard is the Tunnel of a Linux host: it makes the interface over netlink and configures it
// with wgctrl.
type WireGuard struct {
	name   string
	port   int
	public string
	link   netlink.Link
	client *wgctrl.Client
}

// NewWireGuard creates the interface name when it does not exist, brings it up, and gives it the
// private key in keyFile (made on the first start) and the listen port.
func NewWireGuard(name, keyFile string, port int) (*WireGuard, error) {
	key, err := LoadKey(keyFile)
	if err != nil {
		return nil, err
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); !ok {
			return nil, fmt.Errorf("tunnel interface %s: %w", name, err)
		}
		if err := netlink.LinkAdd(&netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: name}}); err != nil {
			return nil, fmt.Errorf("create tunnel interface %s: %w", name, err)
		}
		if link, err = netlink.LinkByName(name); err != nil {
			return nil, fmt.Errorf("tunnel interface %s: %w", name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("bring up tunnel interface %s: %w", name, err)
	}
	client, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wgctrl: %w", err)
	}
	if err := client.ConfigureDevice(name, wgtypes.Config{PrivateKey: &key, ListenPort: &port}); err != nil {
		client.Close()
		return nil, fmt.Errorf("configure tunnel interface %s: %w", name, err)
	}
	return &WireGuard{name: name, port: port, public: key.PublicKey().String(), link: link, client: client}, nil
}

// PublicKey implements Tunnel.
func (w *WireGuard) PublicKey() string { return w.public }

// Port implements Tunnel.
func (w *WireGuard) Port() int { return w.port }

// Close releases the wgctrl client; the interface stays.
func (w *WireGuard) Close() error { return w.client.Close() }

// Set implements Tunnel.
func (w *WireGuard) Set(peers []Peer) error {
	cfg := wgtypes.Config{ReplacePeers: true}
	want := map[string]netip.Prefix{}
	for _, p := range sortedPeers(peers) {
		key, err := wgtypes.ParseKey(p.PublicKey)
		if err != nil {
			return fmt.Errorf("peer %s: not a WireGuard key", p.Name)
		}
		pc := wgtypes.PeerConfig{PublicKey: key, ReplaceAllowedIPs: true}
		if p.Endpoint.IsValid() {
			pc.Endpoint = net.UDPAddrFromAddrPort(p.Endpoint)
		}
		for _, x := range sortedPrefixes(p.Prefixes) {
			pc.AllowedIPs = append(pc.AllowedIPs, *prefixNet(x))
			want[x.String()] = x
		}
		cfg.Peers = append(cfg.Peers, pc)
	}
	if err := w.client.ConfigureDevice(w.name, cfg); err != nil {
		return fmt.Errorf("configure tunnel interface %s: %w", w.name, err)
	}
	have, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{LinkIndex: w.link.Attrs().Index, Protocol: routeProtocol},
		netlink.RT_FILTER_OIF|netlink.RT_FILTER_PROTOCOL)
	if err != nil {
		return fmt.Errorf("list routes of %s: %w", w.name, err)
	}
	for _, r := range have {
		if r.Dst == nil {
			continue
		}
		if p, ok := netPrefix(r.Dst); !ok || !has(want, p) {
			if err := netlink.RouteDel(&r); err != nil {
				return fmt.Errorf("remove route %s: %w", r.Dst, err)
			}
		}
	}
	for _, p := range want {
		r := &netlink.Route{LinkIndex: w.link.Attrs().Index, Dst: prefixNet(p), Scope: netlink.SCOPE_LINK, Protocol: routeProtocol}
		if err := netlink.RouteReplace(r); err != nil {
			return fmt.Errorf("route %s to %s: %w", p, w.name, err)
		}
	}
	return nil
}

func has(m map[string]netip.Prefix, p netip.Prefix) bool {
	_, ok := m[p.String()]
	return ok
}

func prefixNet(p netip.Prefix) *net.IPNet {
	p = p.Masked()
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func netPrefix(n *net.IPNet) (netip.Prefix, bool) {
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(a.Unmap(), ones), true
}
