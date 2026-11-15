package layout

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// NetworkConfig is the Node's network-config, in the version 1 form qemu-server's nocloud_network
// writes for a static address, a gateway, nameservers and a search domain. The second card, when
// there is one, has a static address and no gateway, so that the default route stays on the first.
func (n Node) NetworkConfig() ([]byte, error) {
	if err := n.Check(); err != nil {
		return nil, err
	}
	if n.MAC == "" || !n.Gateway.IsValid() || len(n.Nameservers) == 0 {
		return nil, fmt.Errorf("a network-config needs the card's MAC, a gateway and a nameserver")
	}
	var b strings.Builder
	fmt.Fprintf(&b, `version: 1
config:
    - type: physical
      name: eth0
      mac_address: '%s'
      subnets:
        - type: static
          address: '%s'
          netmask: '%s'
          gateway: '%s'
`, n.MAC, n.Address.Addr(), netmask(n.Address), n.Gateway)
	if nic := n.ExtraNIC; nic != nil {
		fmt.Fprintf(&b, `    - type: physical
      name: eth1
      mac_address: '%s'
      subnets:
        - type: static
          address: '%s'
          netmask: '%s'
`, nic.MAC, nic.Address.Addr(), netmask(nic.Address))
	}
	b.WriteString("    - type: nameserver\n      address:\n")
	for _, a := range n.Nameservers {
		fmt.Fprintf(&b, "      - '%s'\n", a)
	}
	if n.Search != "" {
		fmt.Fprintf(&b, "      search:\n      - '%s'\n", n.Search)
	}
	return []byte(b.String()), nil
}

func netmask(p netip.Prefix) string {
	return net.IP(net.CIDRMask(p.Bits(), 32)).String()
}
