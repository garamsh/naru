package config

import (
	"strconv"
)

// routes checks spec.network.routes: each destination is an IPv4 prefix in its canonical form and
// not the default route, which the network-config owns, each next hop is a unicast IPv4 address
// outside its destination, and no destination is listed twice.
func (v *validation) routes(routes []Route) {
	seen := map[string]bool{}
	for i, r := range routes {
		field := "spec.network.routes[" + strconv.Itoa(i) + "]"
		toOK := false
		switch {
		case !r.To.IsValid() || !r.To.Addr().Is4():
			v.fail(field+".to", "must be an IPv4 prefix such as 10.200.2.0/24, got %q", r.To.String())
		case r.To.Bits() == 0:
			v.fail(field+".to", "must not be the default route, which the network-config gives")
		case r.To != r.To.Masked():
			v.fail(field+".to", "must have no host bits set, got %s; write %s", r.To, r.To.Masked())
		case seen[r.To.String()]:
			v.fail(field+".to", "%s is listed twice", r.To)
		default:
			seen[r.To.String()] = true
			toOK = true
		}
		switch {
		case !r.Via.IsValid() || !r.Via.Is4():
			v.fail(field+".via", "must be an IPv4 address, got %q", r.Via.String())
		case !r.Via.IsGlobalUnicast():
			v.fail(field+".via", "must be a unicast address, got %s", r.Via)
		case toOK && r.To.Contains(r.Via):
			v.fail(field+".via", "%s is inside %s, a route cannot go through its own destination", r.Via, r.To)
		}
	}
}
