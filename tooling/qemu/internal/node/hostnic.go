package node

import "fmt"

// The control-plane Node of a gaon cluster has a second NIC on QEMU's user-mode network, whose host
// forward carries the test host to the API server. naructl is told its address and MAC (gen node
// --extra-nic); the layout of the Node itself is naructl's (ADR 0015).
const (
	// HostNICAddress is the address of the second NIC. The network is 10.0.2.0/24 with the host
	// side at 10.0.2.2, and the guest has no route off it.
	HostNICAddress = "10.0.2.15"
	// HostNICPrefix is HostNICAddress with its network's prefix length.
	HostNICPrefix = HostNICAddress + "/24"
	// APIPort is the TCP port the forward reaches: the one gaon-apiserver serves on, which naructl's
	// layout fixes. The harness test of a cluster fails when the two differ.
	APIPort = "6443"
	// NodeAPIPort is the port init's Node API listens on by default (ADR 0017), which the second
	// forward of a cluster that has spec.api reaches.
	NodeAPIPort = "50000"
)

// HostMAC is the address of the second NIC: the form of the first, in the next block of the
// locally administered range so that no two cards of a cluster share one.
func (c Config) HostMAC() string {
	return fmt.Sprintf("02:00:00:01:%02x:%02x", c.Index>>8, c.Index&0xff)
}

// hostNIC reports whether the Node has the second NIC.
func (c Config) hostNIC() bool { return c.HostPort != 0 }
