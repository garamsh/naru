// Package prefixes allocates the IPv4 prefixes orch hands out: a host's Machine prefix from the
// profile's pool and a member's Pod prefix from its network. Both spaces use one allocator, which
// never returns a prefix that overlaps one already claimed.
package prefixes

import (
	"errors"
	"fmt"
	"net/netip"
)

// ErrFull is a space with no prefix of the asked length left that overlaps no claimed prefix.
var ErrFull = errors.New("no free prefix is left")

// maxScan bounds how many candidates Allocate looks at before it calls the space full.
const maxScan = 1 << 20

// Overlaps reports whether p overlaps any of claimed.
func Overlaps(claimed []netip.Prefix, p netip.Prefix) bool {
	for _, c := range claimed {
		if c.Overlaps(p) {
			return true
		}
	}
	return false
}

// Allocate returns the first prefix of length bits inside space that overlaps none of claimed.
// A caller that keeps part of the space for itself lists that part in claimed.
func Allocate(space netip.Prefix, bits int, claimed []netip.Prefix) (netip.Prefix, error) {
	if !space.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%s is not an IPv4 prefix", space)
	}
	if bits < space.Bits() || bits > 32 {
		return netip.Prefix{}, fmt.Errorf("a /%d does not fit inside %s", bits, space)
	}
	step := uint64(1) << (32 - bits)
	first := uint64(addrUint(space.Masked().Addr()))
	count := uint64(1) << (bits - space.Bits())
	for i := uint64(0); i < min(count, maxScan); i++ {
		c := netip.PrefixFrom(uintAddr(uint32(first+i*step)), bits)
		if !Overlaps(claimed, c) {
			return c, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("%w: %s has no free /%d", ErrFull, space, bits)
}

func addrUint(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func uintAddr(u uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)})
}
