package network

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Ruleset returns the nftables script that replaces the host's table "naru": one atomic
// `nft -f` run. The forward chain drops by default; it accepts the replies of a flow that was
// allowed, a network's traffic inside its own prefix, a network's traffic to the egress, and the
// management plane's traffic to a network's API endpoint. A network's traffic to another network, to
// the host's addresses and to the management prefix is dropped and counted by a rule of its own.
func Ruleset(c Config) string {
	var b strings.Builder
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	line("add table inet naru")
	line("delete table inet naru")
	line("table inet naru {")

	nets := sortedNetworks(c.Networks)

	if c.TapPrefix != "" {
		var src []string
		for _, m := range c.Members {
			src = append(src, m.Address.String())
			if m.Routed.IsValid() {
				src = append(src, m.Routed.String())
			}
		}
		sort.Strings(src)
		if len(src) > 0 {
			line("\tset members {")
			line("\t\ttype ipv4_addr")
			line("\t\tflags interval")
			line("\t\telements = { %s }", strings.Join(src, ", "))
			line("\t}")
		}
	}

	line("\tchain input {")
	line("\t\ttype filter hook input priority 0; policy accept;")
	if len(c.HostAddrs) > 0 {
		for _, n := range nets {
			line("\t\tip saddr %s ip daddr { %s } counter drop comment \"%s to the host\"", n.Prefix, addrList(c.HostAddrs), n.Name)
		}
	}
	line("\t}")

	line("\tchain forward {")
	line("\t\ttype filter hook forward priority 0; policy drop;")
	line("\t\tct state established,related accept")
	if c.TapPrefix != "" {
		if len(c.Members) > 0 {
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
		var others []string
		for _, o := range nets {
			if o.Name != n.Name {
				others = append(others, o.Prefix.String())
			}
		}
		if len(others) > 0 {
			line("\t\tip saddr %s ip daddr { %s } counter drop comment \"%s to another network\"", n.Prefix, strings.Join(others, ", "), n.Name)
		}
		if c.Management.IsValid() {
			line("\t\tip saddr %s ip daddr %s counter drop comment \"%s to management\"", n.Prefix, c.Management, n.Name)
		}
		line("\t\tip saddr %s ip daddr %s accept comment \"%s inside its prefix\"", n.Prefix, n.Prefix, n.Name)
		if c.Egress != "" {
			line("\t\tip saddr %s oifname \"%s\" accept comment \"%s egress\"", n.Prefix, c.Egress, n.Name)
		}
	}
	line("\t}")
	line("}")
	return b.String()
}

func addrList(in []netip.Addr) string {
	s := make([]string, len(in))
	for i, a := range in {
		s[i] = a.String()
	}
	sort.Strings(s)
	return strings.Join(s, ", ")
}
