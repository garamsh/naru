package network

import (
	"net/netip"
	"time"
)

// Link is the LAN interface: the addresses the host owns on it and its announcements.
type Link interface {
	// Check refuses an address the host must not take for a lease: one the host already holds on any
	// interface, the network or broadcast address of the LAN interface's network, and a default
	// gateway. An address a lease holds already passes.
	Check(a netip.Addr) error
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
