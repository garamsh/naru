package network

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// setName is the nftables set of a network. A network's name is a DNS label, which has no
// underscore, so the mapping is one to one.
func setName(network string) string { return "net_" + strings.ReplaceAll(network, "-", "_") }

// elements returns the prefixes with each one that another contains removed, sorted: nftables
// refuses overlapping intervals in one set.
func elements(in []netip.Prefix) []string {
	var out []netip.Prefix
	for _, p := range in {
		p = p.Masked()
		covered := false
		for _, q := range in {
			q = q.Masked()
			if q != p && q.Bits() <= p.Bits() && q.Contains(p.Addr()) {
				covered = true
				break
			}
		}
		if !covered && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	s := make([]string, len(out))
	for i, p := range sortedPrefixes(out) {
		s[i] = elementText(p)
	}
	return s
}

// elementText writes a /32 as the address, which is how nft prints it.
func elementText(p netip.Prefix) string {
	if p.IsSingleIP() {
		return p.Addr().String()
	}
	return p.String()
}

// Ruleset returns the nftables script that replaces the host's table "naru": one atomic
// `nft -f` run. Each network is a set of its prefix, its local members and its remote members. The
// forward chain drops by default; it accepts the replies of a flow that was allowed, a network's
// traffic inside its set, a network's traffic to the egress, and the management plane's traffic to a
// network's API endpoint. A network's traffic to another network's set, to the management prefix
// and from a tap with a source no local member holds is dropped and counted by a rule of its own.
// The input chain drops and counts a new connection from a tap, or from a network's member
// arriving by the tunnel, to any address of the host.
func Ruleset(c Config) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	line("add table inet naru")
	line("delete table inet naru")
	line("table inet naru {")
	leasedSet(c, line)

	nets := sortedNetworks(c.Networks)
	local := map[string][]netip.Prefix{}
	var all, members []netip.Prefix
	for _, n := range nets {
		set := append([]netip.Prefix{n.Prefix}, n.Remote...)
		for _, m := range c.Members {
			if m.Network != n.Name {
				continue
			}
			set = append(set, netip.PrefixFrom(m.Address, 32))
			members = append(members, netip.PrefixFrom(m.Address, 32))
			if m.Routed.IsValid() {
				set = append(set, m.Routed)
				members = append(members, m.Routed)
			}
		}
		local[n.Name] = set
		all = append(all, set...)
	}
	set := func(name string, in []netip.Prefix) {
		line("\tset %s {", name)
		line("\t\ttype ipv4_addr")
		line("\t\tflags interval")
		line("\t\telements = { %s }", strings.Join(elements(in), ", "))
		line("\t}")
	}
	for _, n := range nets {
		set(setName(n.Name), local[n.Name])
	}
	if len(nets) > 0 {
		set("networks", all)
	}
	if c.TapPrefix != "" && len(members) > 0 {
		set("members", members)
	}

	line("\tchain input {")
	line("\t\ttype filter hook input priority 0; policy accept;")
	line("\t\tct state established,related accept")
	if c.TapPrefix != "" {
		line("\t\tiifname \"%s*\" counter drop comment \"microVM to the host\"", c.TapPrefix)
	}
	for _, n := range nets {
		line("\t\tip saddr @%s counter drop comment \"%s to the host\"", setName(n.Name), n.Name)
	}
	line("\t}")

	line("\tchain forward {")
	line("\t\ttype filter hook forward priority 0; policy drop;")
	line("\t\tct state established,related accept")
	exposureForward(c, line)
	if c.TapPrefix != "" {
		if len(members) > 0 {
			line("\t\tiifname \"%s*\" ip saddr != @members counter drop comment \"forged source\"", c.TapPrefix)
		} else {
			line("\t\tiifname \"%s*\" counter drop comment \"forged source\"", c.TapPrefix)
		}
	}
	for _, n := range nets {
		if c.Management.IsValid() && n.Endpoint.IsValid() {
			line("\t\tip saddr %s ip daddr %s accept comment \"management to %s's API endpoint\"", c.Management, n.Endpoint, n.Name)
		}
	}
	for _, n := range nets {
		s := setName(n.Name)
		line("\t\tip saddr @%s ip daddr @%s accept comment \"%s inside its set\"", s, s, n.Name)
		line("\t\tip saddr @%s ip daddr @networks counter drop comment \"%s to another network\"", s, n.Name)
		if c.Management.IsValid() {
			line("\t\tip saddr @%s ip daddr %s counter drop comment \"%s to management\"", s, c.Management, n.Name)
		}
		if c.Egress != "" {
			line("\t\tip saddr @%s oifname \"%s\" accept comment \"%s egress\"", s, c.Egress, n.Name)
		}
	}
	line("\t}")
	exposureRules(c, line)
	line("}")
	return b.String()
}

// exposureForward writes the rules of the forward chain that let the LAN reach the backends' NodePorts
// of the exposures, and nothing else: one rule per forwarded port, from the LAN interface, to that
// exposure's backends, for a flow the DNAT made.
func exposureForward(c Config, line func(string, ...any)) {
	if c.LAN == "" {
		return
	}
	for _, e := range sortedExposures(c.Exposures) {
		for _, p := range e.forwarded() {
			line("\t\tiifname \"%s\" ip daddr { %s } %s dport %d ct status dnat ct original ip daddr @leased ct state new accept comment \"%s:%d\"", c.LAN, addrList(e.Backends), p.Protocol, p.NodePort, e.Address, p.Port)
		}
	}
}

// exposureRules writes the chains that forward the LAN's traffic to the exposures' backends (ADR
// 0085 item 6): a DNAT that picks a backend at random and a source NAT so the reply returns by this
// host.
func exposureRules(c Config, line func(string, ...any)) {
	if c.LAN == "" {
		return
	}
	var forwarded []Exposure
	for _, e := range sortedExposures(c.Exposures) {
		if len(e.forwarded()) > 0 {
			forwarded = append(forwarded, e)
		}
	}
	if len(forwarded) == 0 {
		return
	}
	line("\tchain exposure_dnat {")
	line("\t\ttype nat hook prerouting priority dstnat; policy accept;")
	for _, e := range forwarded {
		for _, p := range e.forwarded() {
			line("\t\tiifname \"%s\" ip daddr %s ip daddr @leased %s dport %d dnat ip to numgen random mod %d map { %s } comment \"%s\"",
				c.LAN, e.Address, p.Protocol, p.Port, len(e.Backends), backendMap(e.Backends, p.NodePort), e.Network)
		}
	}
	line("\t}")
	line("\tchain exposure_snat {")
	line("\t\ttype nat hook postrouting priority srcnat; policy accept;")
	// Only a flow to a leased address is source-NATed, not every flow the DNAT of this table made.
	line("\t\tiifname \"%s\" ct status dnat ct original ip daddr @leased masquerade", c.LAN)
	line("\t}")
}

func addrList(in []netip.Addr) string {
	s := make([]string, len(in))
	for i, a := range in {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// backendMap is the map a numgen picks from: number i is backend i at the NodePort.
func backendMap(backends []netip.Addr, nodePort int) string {
	s := make([]string, len(backends))
	for i, a := range backends {
		s[i] = fmt.Sprintf("%d : %s . %d", i, a, nodePort)
	}
	return strings.Join(s, ", ")
}

// leasedSet writes the set of the leased addresses that DNAT forwards. Each element has the time its
// lease has left as its timeout, so the host stops forwarding an address whose lease is not renewed
// even when hostd has stopped. With no LAN interface there is no set.
func leasedSet(c Config, line func(string, ...any)) {
	if c.LAN == "" {
		return
	}
	line("\tset leased {")
	line("\t\ttype ipv4_addr; flags timeout;")
	if el := leasedElements(c); el != "" {
		line("\t\telements = { %s }", el)
	}
	line("\t}")
}

// leasedElements lists the leased addresses that DNAT forwards, each with the whole seconds its lease
// has left, at least one.
func leasedElements(c Config) string {
	var s []string
	for _, e := range sortedExposures(c.Exposures) {
		if len(e.forwarded()) == 0 {
			continue
		}
		left := int((e.Deadline.Sub(c.At) + time.Second - 1) / time.Second)
		s = append(s, fmt.Sprintf("%s timeout %ds", e.Address, max(left, 1)))
	}
	return strings.Join(s, ", ")
}

// leasedRefresh is the script that sets the time left of every leased address again, in one atomic
// run, and changes no rule.
func leasedRefresh(c Config) string {
	if c.LAN == "" {
		return ""
	}
	out := "flush set inet naru leased\n"
	if el := leasedElements(c); el != "" {
		out += "add element inet naru leased { " + el + " }\n"
	}
	return out
}
