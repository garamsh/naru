package network

import (
	"net/netip"
	"time"
)

// Link is the LAN interface: the addresses the host owns on it and its announcements.
type Link interface {
	// Check refuses an address the host must not take for a new lease: one the host already holds on
	// any interface, the LAN's network or broadcast address, a default gateway, or an address not
	// inside any prefix of the LAN interface. An address a lease already holds passes.
	Check(a netip.Addr) error
	// Probe sends an ARP probe for a and returns ErrTaken if any host answers within the probe
	// window. Call it for new leases only; renewals do not probe.
	Probe(a netip.Addr) error
	// Add gives the interface the address as a /32 that the kernel removes after valid, and sets the
	// time again when the address is held. It refuses an address the host holds for another purpose.
	Add(a netip.Addr, valid time.Duration) error
	// Remove takes the address off; an absent one is not an error.
	Remove(a netip.Addr) error
	// Held lists the addresses Add put on the interface and Remove has not taken off, including those
	// of an earlier run of the daemon.
	Held() ([]netip.Addr, error)
	// Announce sends an unsolicited ARP reply for the address.
	Announce(a netip.Addr) error
}
