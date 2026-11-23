package groups

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// validAddresses reports whether every address is an IPv4 address in its canonical form and none
// is listed twice.
func validAddresses(addrs []string) bool {
	seen := map[netip.Addr]bool{}
	for _, s := range addrs {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() || a.String() != s || seen[a] {
			return false
		}
		seen[a] = true
	}
	return true
}

// memberAddress is the address the template fixes for the member named id of group: the member
// <group>-<n> has Addresses[n-1]. ok is false for a template that fixes none; err is set when it
// fixes some and none for this member.
func memberAddress(t Template, group, id string) (addr netip.Addr, ok bool, err error) {
	if len(t.Addresses) == 0 || t.Form != FormOS {
		return netip.Addr{}, false, nil
	}
	n, perr := strconv.Atoi(strings.TrimPrefix(id, group+"-"))
	if perr != nil || n < 1 || n > len(t.Addresses) {
		return netip.Addr{}, false, fmt.Errorf("member %s has no address: the template lists %d, one for each of the members %s-1 to %s-%d", id, len(t.Addresses), group, group, len(t.Addresses))
	}
	return netip.MustParseAddr(t.Addresses[n-1]), true, nil
}

// addressGaps is what keeps a host from taking the member whose address is addr: the address is not
// inside any Machine prefix the host gives addresses from. A host that reports no prefix is not
// refused: it is the older host API, whose own create is the check.
func addressGaps(addr netip.Addr, h Host) []string {
	if len(h.MachinePrefixes) == 0 {
		return nil
	}
	for _, p := range h.MachinePrefixes {
		if pf, err := netip.ParsePrefix(p); err == nil && pf.Contains(addr) {
			return nil
		}
	}
	return []string{fmt.Sprintf("address %s is outside the host's Machine prefixes %v", addr, h.MachinePrefixes)}
}

// addressOf is the address the template fixes for the member id as text, or empty for none. Place
// has refused a member the template gives no address, so only a placed member reaches it.
func addressOf(spec GroupSpec, id string) string {
	if a, ok, _ := memberAddress(spec.Template, spec.Name, id); ok {
		return a.String()
	}
	return ""
}
