package netcfg_test

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/netcfg"
)

// proxmox is what qemu-server's nocloud_network writes for a guest with one
// static address, a gateway, a nameserver and a search domain.
const proxmox = `version: 1
config:
    - type: physical
      name: eth0
      mac_address: '02:00:00:00:00:01'
      subnets:
        - type: static
          address: '10.78.0.1'
          netmask: '255.255.255.0'
          gateway: '10.78.0.254'
    - type: nameserver
      address:
      - '10.78.0.254'
      search:
      - 'naru.test'
`

func TestParse_ReadsWhatProxmoxWrites(t *testing.T) {
	cfg, err := netcfg.Parse([]byte(proxmox))

	require.NoError(t, err)
	want := netcfg.Config{
		Interfaces: []netcfg.Interface{{
			Name:      "eth0",
			MAC:       net.HardwareAddr{0x02, 0, 0, 0, 0, 0x01},
			Addresses: []netip.Prefix{netip.MustParsePrefix("10.78.0.1/24")},
			Gateway:   netip.MustParseAddr("10.78.0.254"),
		}},
		Nameservers: []netip.Addr{netip.MustParseAddr("10.78.0.254")},
		Search:      []string{"naru.test"},
	}
	assert.Equal(t, want, cfg)
}

func TestParse_ReadsAnAddressWithAPrefixLengthAndAnInterfaceKnownByName(t *testing.T) {
	cfg, err := netcfg.Parse([]byte(`version: 1
config:
  - type: physical
    name: eth1
    subnets:
      - type: static
        address: 192.168.1.5/25
`))

	require.NoError(t, err)
	require.Len(t, cfg.Interfaces, 1)
	iface := cfg.Interfaces[0]
	assert.Equal(t, "eth1", iface.Name)
	assert.Nil(t, iface.MAC)
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("192.168.1.5/25")}, iface.Addresses)
	assert.False(t, iface.Gateway.IsValid())
}

func TestParse_RefusesWhatItCannotApply(t *testing.T) {
	const head = "version: 1\nconfig:\n"
	const nic = "  - type: physical\n    name: eth0\n    mac_address: '02:00:00:00:00:01'\n    subnets:\n"
	tests := map[string]struct {
		doc  string
		want string
	}{
		"not YAML":                     {"config: [", "parse network-config"},
		"version 2":                    {"version: 2\nconfig: []\n", "version must be 1"},
		"no version":                   {"config: []\n", "version must be 1"},
		"an unknown key":               {head + nic + "      - type: static\n        address: 10.0.0.1/24\n        dns: x\n", "dns"},
		"dhcp4":                        {head + nic + "      - type: dhcp4\n", `type "dhcp4" is not supported`},
		"dhcp6":                        {head + nic + "      - type: dhcp6\n", `type "dhcp6" is not supported`},
		"slaac":                        {head + nic + "      - type: ipv6_slaac\n", `type "ipv6_slaac" is not supported`},
		"static6":                      {head + nic + "      - type: static6\n        address: 'fd00::1/64'\n", `type "static6" is not supported`},
		"a bond":                       {head + "  - type: bond\n    name: bond0\n", `type "bond" is not supported`},
		"an IPv6 static address":       {head + nic + "      - type: static\n        address: 'fd00::1/64'\n", "not an IPv4 address"},
		"no netmask":                   {head + nic + "      - type: static\n        address: 10.0.0.1\n", "netmask"},
		"a netmask with holes":         {head + nic + "      - type: static\n        address: 10.0.0.1\n        netmask: 255.0.255.0\n", "not contiguous"},
		"a prefix given twice":         {head + nic + "      - type: static\n        address: 10.0.0.1/24\n        netmask: 255.255.255.0\n", "twice"},
		"a gateway off the link":       {head + nic + "      - type: static\n        address: 10.0.0.1/24\n        gateway: 10.9.0.1\n", "outside 10.0.0.0/24"},
		"no subnets":                   {head + "  - type: physical\n    name: eth0\n", "no subnets"},
		"no interface named":           {head + "  - type: physical\n    subnets:\n      - type: static\n        address: 10.0.0.1/24\n", "names no interface"},
		"a bad MAC":                    {head + "  - type: physical\n    mac_address: zz\n    subnets:\n      - type: static\n        address: 10.0.0.1/24\n", "not an Ethernet address"},
		"a bad nameserver":             {head + nic + "      - type: static\n        address: 10.0.0.1/24\n  - type: nameserver\n    address: [not-an-ip]\n", "not an IP address"},
		"a search domain with a space": {head + nic + "      - type: static\n        address: 10.0.0.1/24\n  - type: nameserver\n    search: ['a b']\n", "not a domain name"},
		"two nameserver entries":       {head + nic + "      - type: static\n        address: 10.0.0.1/24\n  - type: nameserver\n  - type: nameserver\n", "second nameserver"},
		"no interface at all":          {head + "  - type: nameserver\n    address: [10.0.0.1]\n", "no interface"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := netcfg.Parse([]byte(tt.doc))

			assert.ErrorContains(t, err, tt.want)
		})
	}
}
