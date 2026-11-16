package linux

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/netcfg"
)

// resolvConf is where init writes the resolvers. /etc/resolv.conf in the
// image is a symbolic link to it, because the root is read-only.
const resolvConf = "/run/naru/resolv.conf"

// link is a network interface as ApplyNetwork needs it.
type link struct {
	Index int
	Name  string
	MAC   net.HardwareAddr
	// Loopback is the kernel's IFF_LOOPBACK flag. The kernel gives lo a 6-byte zero MAC, so the
	// MAC does not tell it from an Ethernet link.
	Loopback bool
}

// links is the kernel's network interfaces, reached through netlink.
type links interface {
	List() ([]link, error)
	SetUp(link) error
	// Carrier reports whether the link sees a carrier.
	Carrier(link) bool
	AddAddr(link, netip.Prefix) error
	DelAddr(link, netip.Prefix) error
	SetDefaultRoute(link, netip.Addr) error
	DeleteDefaultRoute(link, netip.Addr) error
	// ReplaceRoute installs the route to a prefix through a next hop, and DeleteRoute removes it;
	// the kernel picks the device the next hop is on.
	ReplaceRoute(netip.Prefix, netip.Addr) error
	DeleteRoute(netip.Prefix, netip.Addr) error
}

// AddRoute installs the static route r, replacing one to the same prefix. It fails when the
// kernel has no network the next hop is on.
func (h *Host) AddRoute(r config.Route) error {
	if err := h.links.ReplaceRoute(r.To, r.Via); err != nil {
		return fmt.Errorf("route %s via %s: %w", r.To, r.Via, err)
	}
	return nil
}

// DeleteRoute removes the static route r. A route that is not there is not an error.
func (h *Host) DeleteRoute(r config.Route) error {
	if err := h.links.DeleteRoute(r.To, r.Via); err != nil {
		return fmt.Errorf("delete route %s via %s: %w", r.To, r.Via, err)
	}
	return nil
}

// ApplyNetwork brings each interface of the configuration up with its
// addresses and default gateway, and writes the resolvers. An interface that
// has not appeared when it is first looked for is waited for up to diskWait.
// An interface that is a DHCP one gets its address, default route and resolvers
// from a DHCPv4 lease, which RunNetwork keeps; an interface with neither name
// nor MAC is the first Ethernet link with a carrier. It returns what the Node
// was given: its addresses, its resolvers and the time servers of the lease.
func (h *Host) ApplyNetwork(cfg netcfg.Config, log *slog.Logger) (netcfg.Applied, error) {
	var applied netcfg.Applied
	// The kernel brings lo up for ip= but for nothing else, and a service that listens on 127.0.0.1 needs it.
	lo, err := h.findLink(netcfg.Interface{Name: "lo"})
	if err != nil {
		return applied, err
	}
	if err := h.links.SetUp(lo); err != nil {
		return applied, fmt.Errorf("bring lo up: %w", err)
	}
	var leased []netip.Addr
	for _, want := range cfg.Interfaces {
		var l link
		if want.Name == "" && want.MAC == nil {
			l, err = h.firstCarrierLink()
		} else {
			l, err = h.findLink(want)
		}
		if err != nil {
			return applied, err
		}
		if err := h.links.SetUp(l); err != nil {
			return applied, fmt.Errorf("bring %s up: %w", l.Name, err)
		}
		if want.DHCP {
			lease, err := h.startLease(l, cfg, log)
			if err != nil {
				return applied, err
			}
			applied.Addresses = append(applied.Addresses, lease.Address.Addr())
			applied.NTP = append(applied.NTP, lease.NTP...)
			leased = lease.DNS
			continue
		}
		for _, a := range want.Addresses {
			if err := h.links.AddAddr(l, a); err != nil {
				return applied, fmt.Errorf("add %s to %s: %w", a, l.Name, err)
			}
			applied.Addresses = append(applied.Addresses, a.Addr())
		}
		if want.Gateway.IsValid() {
			if err := h.links.SetDefaultRoute(l, want.Gateway); err != nil {
				return applied, fmt.Errorf("default route via %s on %s: %w", want.Gateway, l.Name, err)
			}
		}
	}
	applied.Nameservers = resolvers(cfg, leased)
	return applied, h.writeResolvConf(cfg.Search, applied.Nameservers)
}

// resolvers are the nameserver entry of network-config when it has one, and the lease's
// otherwise.
func resolvers(cfg netcfg.Config, leased []netip.Addr) []netip.Addr {
	if len(cfg.Nameservers) > 0 {
		return cfg.Nameservers
	}
	return leased
}

// firstCarrierLink brings every Ethernet link up, loopback excluded, and returns the first one that sees a carrier,
// waiting up to carrierWait for one.
func (h *Host) firstCarrierLink() (link, error) {
	deadline := time.Now().Add(h.carrierWait)
	for {
		all, err := h.links.List()
		if err != nil {
			return link{}, fmt.Errorf("list network interfaces: %w", err)
		}
		for _, l := range all {
			if !isEthernet(l) {
				continue
			}
			if err := h.links.SetUp(l); err != nil {
				return link{}, fmt.Errorf("bring %s up: %w", l.Name, err)
			}
		}
		for _, l := range all {
			if isEthernet(l) && h.links.Carrier(l) {
				return l, nil
			}
		}
		if time.Now().After(deadline) {
			return link{}, errors.New("no Ethernet link has a carrier")
		}
		time.Sleep(diskPollInterval)
	}
}

// isEthernet reports whether l is a link a DHCP client can lease on: it has a MAC and is not
// loopback.
func isEthernet(l link) bool { return len(l.MAC) == 6 && !l.Loopback }

// findLink returns the interface with the MAC of want, or with its name when
// it has no MAC.
func (h *Host) findLink(want netcfg.Interface) (link, error) {
	deadline := time.Now().Add(h.diskWait)
	for {
		all, err := h.links.List()
		if err != nil {
			return link{}, fmt.Errorf("list network interfaces: %w", err)
		}
		for _, l := range all {
			if want.MAC != nil && bytes.Equal(l.MAC, want.MAC) || want.MAC == nil && l.Name == want.Name {
				return l, nil
			}
		}
		if time.Now().After(deadline) {
			if want.MAC != nil {
				return link{}, fmt.Errorf("no network interface has the MAC %s", want.MAC)
			}
			return link{}, fmt.Errorf("no network interface is named %q", want.Name)
		}
		time.Sleep(diskPollInterval)
	}
}

func resolvContent(search []string, nameservers []netip.Addr) string {
	var b bytes.Buffer
	if len(search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(search, " "))
	}
	for _, ns := range nameservers {
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	return b.String()
}

func (h *Host) writeResolvConf(search []string, nameservers []netip.Addr) error {
	return writeFile(h.resolvConf, []byte(resolvContent(search, nameservers)), 0o644)
}

// WriteFiles writes the files of the configuration, creating their directories.
func (h *Host) WriteFiles(files []config.File) error {
	for _, f := range files {
		if err := writeFile(f.Path, []byte(f.Content), f.Perm()); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
		if f.Owner != nil {
			if err := os.Lchown(f.Path, int(f.Owner.UID), int(f.Owner.GID)); err != nil {
				return fmt.Errorf("own %s: %w", f.Path, err)
			}
		}
	}
	return nil
}

// ReplaceFile writes the file of the configuration over the one at its path,
// or creates it, so a reader sees the old file or the new one and never a
// partial one. It creates the file's directories.
func (h *Host) ReplaceFile(f config.File) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	var opts []atomicfile.Option
	if f.Owner != nil {
		opts = append(opts, atomicfile.Owner(int(f.Owner.UID), int(f.Owner.GID)))
	}
	if err := atomicfile.Write(f.Path, []byte(f.Content), f.Perm(), opts...); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	return nil
}

// writeFile creates path, and its directories, with exactly the mode given and
// refuses to replace a file that exists.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// netlinkLinks is links over the kernel's netlink socket.
type netlinkLinks struct{}

func (netlinkLinks) List() ([]link, error) {
	all, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	out := make([]link, 0, len(all))
	for _, l := range all {
		a := l.Attrs()
		out = append(out, link{Index: a.Index, Name: a.Name, MAC: a.HardwareAddr, Loopback: a.RawFlags&unix.IFF_LOOPBACK != 0})
	}
	return out, nil
}

func (netlinkLinks) SetUp(l link) error { return netlink.LinkSetUp(handle(l)) }

func (netlinkLinks) Carrier(l link) bool {
	got, err := netlink.LinkByIndex(l.Index)
	return err == nil && got.Attrs().RawFlags&unix.IFF_LOWER_UP != 0
}

func (netlinkLinks) AddAddr(l link, p netip.Prefix) error {
	return netlink.AddrReplace(handle(l), &netlink.Addr{IPNet: ipNet(p)})
}

func (netlinkLinks) DelAddr(l link, p netip.Prefix) error {
	err := netlink.AddrDel(handle(l), &netlink.Addr{IPNet: ipNet(p)})
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return nil
	}
	return err
}

func (netlinkLinks) SetDefaultRoute(l link, gw netip.Addr) error {
	return netlink.RouteReplace(&netlink.Route{LinkIndex: l.Index, Gw: gw.AsSlice()})
}

func (netlinkLinks) DeleteDefaultRoute(l link, gw netip.Addr) error {
	err := netlink.RouteDel(&netlink.Route{LinkIndex: l.Index, Gw: gw.AsSlice()})
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func (netlinkLinks) ReplaceRoute(to netip.Prefix, via netip.Addr) error {
	return netlink.RouteReplace(&netlink.Route{Dst: ipNet(to), Gw: via.AsSlice(), Protocol: syscall.RTPROT_STATIC})
}

func (netlinkLinks) DeleteRoute(to netip.Prefix, via netip.Addr) error {
	err := netlink.RouteDel(&netlink.Route{Dst: ipNet(to), Gw: via.AsSlice(), Protocol: syscall.RTPROT_STATIC})
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), 8*len(p.Addr().AsSlice()))}
}

func handle(l link) netlink.Link {
	return &netlink.Device{LinkAttrs: netlink.LinkAttrs{Index: l.Index, Name: l.Name}}
}
