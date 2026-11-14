package groups

import (
	"fmt"
	"net/netip"
)

// podPrefixes is the Pod prefixes the members hold, per network, while one pass plans. A member
// holds its prefix until its record is gone, so a prefix is never planned for a second member.
type podPrefixes struct {
	held map[string][]netip.Prefix
}

func newPodPrefixes(members []Member) *podPrefixes {
	p := &podPrefixes{held: map[string][]netip.Prefix{}}
	for _, m := range members {
		if m.Phase == PhaseGone || m.PodPrefix == "" {
			continue
		}
		if pfx, err := netip.ParsePrefix(m.PodPrefix); err == nil {
			p.held[m.Network] = append(p.held[m.Network], pfx)
		}
	}
	return p
}

// maxScan bounds how many candidates take looks at before it calls the network exhausted.
const maxScan = 1 << 20

// take returns the first prefix of the given length inside the network's prefix that overlaps none
// held, and records it as held.
func (p *podPrefixes) take(n Network, bits int) (netip.Prefix, error) {
	space, err := netip.ParsePrefix(n.Prefix)
	if err != nil || !space.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("network %s has prefix %q, which is not an IPv4 prefix", n.Name, n.Prefix)
	}
	if bits <= space.Bits() || bits > 32 {
		return netip.Prefix{}, fmt.Errorf("pod prefix length %d does not fit inside %s of network %s", bits, space, n.Name)
	}
	step := uint32(1) << (32 - bits)
	first := addrUint(space.Addr())
	count := uint64(1) << (bits - space.Bits())
	for i := uint64(0); i < min(count, maxScan); i++ {
		c := netip.PrefixFrom(uintAddr(first+uint32(i)*step), bits)
		if !p.overlaps(n.Name, c) {
			p.held[n.Name] = append(p.held[n.Name], c)
			return c, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("network %s has no free /%d prefix left in %s", n.Name, bits, space)
}

func (p *podPrefixes) overlaps(network string, c netip.Prefix) bool {
	for _, h := range p.held[network] {
		if h.Overlaps(c) {
			return true
		}
	}
	return false
}

func addrUint(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uintAddr(u uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)})
}
