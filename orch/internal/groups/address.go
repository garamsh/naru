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

// Want is the address a template fixes for one member, worked out once when the member is planned:
// Address is valid when the template fixes one, and Err is set when it fixes some and none for this
// member.
type Want struct {
	Address netip.Addr
	Err     error
}

// WantOf is what the template of spec fixes for the member named id: the member <group>-<n> has
// Addresses[n-1]. A template that fixes none, or is not the OS form, wants nothing.
func WantOf(spec GroupSpec, id string) Want {
	t := spec.Template
	if len(t.Addresses) == 0 || t.Form != FormOS {
		return Want{}
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, spec.Name+"-"))
	if err != nil || n < 1 || n > len(t.Addresses) {
		return Want{Err: fmt.Errorf("member %s has no address: the template lists %d, one for each of the members %s-1 to %s-%d", id, len(t.Addresses), spec.Name, spec.Name, len(t.Addresses))}
	}
	return Want{Address: netip.MustParseAddr(t.Addresses[n-1])}
}

// String is the address as text, or empty for none.
func (w Want) String() string {
	if !w.Address.IsValid() {
		return ""
	}
	return w.Address.String()
}

// gaps is what keeps a host from taking the member: the template gives it no address, or the address
// is not inside any Machine prefix the host gives addresses from. A host that reports no prefix is
// not refused: it is the older host API, whose own create is the check.
func (w Want) gaps(h Host) []string {
	switch {
	case w.Err != nil:
		return []string{w.Err.Error()}
	case !w.Address.IsValid(), len(h.MachinePrefixes) == 0:
		return nil
	}
	for _, p := range h.MachinePrefixes {
		if pf, err := netip.ParsePrefix(p); err == nil && pf.Contains(w.Address) {
			return nil
		}
	}
	return []string{fmt.Sprintf("address %s is outside the host's Machine prefixes %v", w.Address, h.MachinePrefixes)}
}
