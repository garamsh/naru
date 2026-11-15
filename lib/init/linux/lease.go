package linux

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/dhcp4"
	"github.com/garamsh/naru/lib/init/netcfg"
)

// leaseWait bounds the first DHCPv4 exchange: a Node that gets no lease in it does not boot.
const leaseWait = 90 * time.Second

// realClock is dhcp4.Clock over the system clock.
type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// leaseState is the lease the Node runs on, and what it needs to change the network when the
// lease does. It is a dhcp4.Handler.
type leaseState struct {
	host   *Host
	link   link
	client *dhcp4.Client
	cfg    netcfg.Config
	log    *slog.Logger

	mu    sync.Mutex
	lease dhcp4.Lease
}

// startLease runs the DHCPv4 exchange on l, up to leaseWait, applies the lease and keeps it for
// RunNetwork.
func (h *Host) startLease(l link, cfg netcfg.Config, log *slog.Logger) (dhcp4.Lease, error) {
	open := h.dhcpOpen
	if open == nil {
		open = openPacketTransport
	}
	transport, err := open(l)
	if err != nil {
		return dhcp4.Lease{}, fmt.Errorf("DHCPv4 on %s: %w", l.Name, err)
	}
	var clock dhcp4.Clock = realClock{}
	if h.dhcpClock != nil {
		clock = h.dhcpClock
	}
	s := &leaseState{host: h, link: l, cfg: cfg, log: log}
	s.client = &dhcp4.Client{HWAddr: l.MAC, Transport: transport, Clock: clock, Handler: s, Log: log}
	wait := h.leaseWait
	if wait == 0 {
		wait = leaseWait
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	lease, err := s.client.Acquire(ctx)
	if err != nil {
		return dhcp4.Lease{}, fmt.Errorf("no DHCPv4 lease on %s within %s: %w", l.Name, wait, err)
	}
	if err := s.apply(lease); err != nil {
		return dhcp4.Lease{}, err
	}
	s.mu.Lock()
	s.lease = lease
	s.mu.Unlock()
	h.leases = s
	log.Info("DHCPv4 lease", "link", l.Name, "address", lease.Address, "router", lease.Router, "dns", lease.DNS, "ntp", lease.NTP, "duration", lease.Duration)
	return lease, nil
}

// RunNetwork renews the DHCPv4 lease for as long as ctx lasts, and returns at once when the
// network has none.
func (h *Host) RunNetwork(ctx context.Context) error {
	s := h.leases
	if s == nil {
		return nil
	}
	s.mu.Lock()
	lease := s.lease
	s.mu.Unlock()
	return s.client.Run(ctx, lease)
}

// apply puts the lease's address, default route and resolvers on the link.
func (s *leaseState) apply(l dhcp4.Lease) error {
	if err := s.host.links.AddAddr(s.link, l.Address); err != nil {
		return fmt.Errorf("add %s to %s: %w", l.Address, s.link.Name, err)
	}
	if l.Router.IsValid() {
		if err := s.host.links.SetDefaultRoute(s.link, l.Router); err != nil {
			return fmt.Errorf("default route via %s on %s: %w", l.Router, s.link.Name, err)
		}
	}
	return nil
}

func (s *leaseState) writeResolvers(l dhcp4.Lease) error {
	return s.host.ReplaceFile(config.File{
		Path: s.host.resolvConf, Mode: "0644", Content: resolvContent(s.cfg.Search, resolvers(s.cfg, l.DNS)),
	})
}

// Bound makes the network match a new lease: it drops the old address and route when they are not
// the new ones, and writes the resolvers again.
func (s *leaseState) Bound(l dhcp4.Lease) {
	s.mu.Lock()
	old := s.lease
	s.lease = l
	s.mu.Unlock()
	if old.Address != l.Address && old.Address.IsValid() {
		s.remove(old)
	}
	if err := s.apply(l); err != nil {
		s.log.Error("applying the DHCPv4 lease failed", "err", err)
	}
	if err := s.writeResolvers(l); err != nil {
		s.log.Error("writing the resolvers of the DHCPv4 lease failed", "err", err)
	}
	s.log.Info("DHCPv4 lease changed", "address", l.Address, "router", l.Router, "dns", l.DNS, "expires", l.Expires())
}

// Expired removes the address and the default route of a lease that is gone.
func (s *leaseState) Expired(l dhcp4.Lease) {
	s.remove(l)
	s.log.Warn("DHCPv4 lease expired; the address and default route are removed", "address", l.Address)
}

func (s *leaseState) remove(l dhcp4.Lease) {
	if l.Router.IsValid() {
		if err := s.host.links.DeleteDefaultRoute(s.link, l.Router); err != nil {
			s.log.Error("removing the default route failed", "via", l.Router, "err", err)
		}
	}
	if err := s.host.links.DelAddr(s.link, l.Address); err != nil {
		s.log.Error("removing the address failed", "address", l.Address, "err", err)
	}
}
