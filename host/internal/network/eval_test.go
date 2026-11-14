package network_test

import (
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// packet is what the interpreter below judges: where it came in, where it goes out, its addresses,
// and whether it belongs to a flow that was allowed.
type packet struct {
	in, out     string
	src, dst    string
	established bool
}

// verdict is what the first rule that matches decides, with its comment, or the chain's policy.
type verdict struct {
	accept bool
	rule   string
}

type rule struct {
	matches []func(packet) bool
	accept  bool
	comment string
}

type chain struct {
	policy string
	rules  []rule
}

// table is a parsed `table inet naru` script: just the part of nftables the ruleset uses.
type table struct {
	sets   map[string][]netip.Prefix
	chains map[string]*chain
}

var (
	commentRe = regexp.MustCompile(` comment "([^"]*)"$`)
	policyRe  = regexp.MustCompile(`policy (accept|drop);`)
)

func parsePrefix(s string) netip.Prefix {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p
	}
	a := netip.MustParseAddr(s)
	return netip.PrefixFrom(a, a.BitLen())
}

// parseTable reads the script the way nft would, so a test can ask what the host does with a packet.
func parseTable(t *testing.T, script string) *table {
	t.Helper()
	tb := &table{sets: map[string][]netip.Prefix{}, chains: map[string]*chain{}}
	var set string
	var ch *chain
	for _, l := range strings.Split(script, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "set "):
			set = strings.Fields(l)[1]
			tb.sets[set] = nil
		case strings.HasPrefix(l, "elements = {"):
			body := strings.TrimSuffix(strings.TrimPrefix(l, "elements = {"), "}")
			for _, e := range strings.Split(body, ",") {
				if e = strings.TrimSpace(e); e != "" {
					tb.sets[set] = append(tb.sets[set], parsePrefix(e))
				}
			}
		case strings.HasPrefix(l, "chain "):
			ch = &chain{}
			tb.chains[strings.Fields(l)[1]] = ch
		case strings.HasPrefix(l, "type filter"):
			ch.policy = policyRe.FindStringSubmatch(l)[1]
		case ch != nil && (strings.Contains(l, "accept") || strings.Contains(l, "drop")) && l != "}":
			ch.rules = append(ch.rules, tb.parseRule(t, l))
		}
	}
	return tb
}

func in(set []netip.Prefix, a string) bool {
	x := netip.MustParseAddr(a)
	for _, p := range set {
		if p.Contains(x) {
			return true
		}
	}
	return false
}

func (tb *table) parseRule(t *testing.T, l string) rule {
	var r rule
	if m := commentRe.FindStringSubmatch(l); m != nil {
		r.comment = m[1]
		l = strings.TrimSuffix(l, m[0])
	}
	f := strings.Fields(l)
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case "ct":
			r.matches = append(r.matches, func(p packet) bool { return p.established })
			i += 2
		case "iifname", "oifname":
			name := strings.Trim(f[i+1], `"`)
			prefix := strings.HasSuffix(name, "*")
			name = strings.TrimSuffix(name, "*")
			get := func(p packet) string { return p.in }
			if f[i] == "oifname" {
				get = func(p packet) string { return p.out }
			}
			r.matches = append(r.matches, func(p packet) bool {
				if prefix {
					return strings.HasPrefix(get(p), name)
				}
				return get(p) == name
			})
			i++
		case "ip":
			get := func(p packet) string { return p.src }
			if f[i+1] == "daddr" {
				get = func(p packet) string { return p.dst }
			}
			i += 2
			not := f[i] == "!="
			if not {
				i++
			}
			var set []netip.Prefix
			if strings.HasPrefix(f[i], "@") {
				s, ok := tb.sets[f[i][1:]]
				require.True(t, ok, "the set %s is not declared", f[i])
				set = s
			} else {
				set = []netip.Prefix{parsePrefix(f[i])}
			}
			r.matches = append(r.matches, func(p packet) bool { return in(set, get(p)) != not })
		case "counter":
		case "accept":
			r.accept = true
		case "drop":
		default:
			t.Fatalf("the interpreter does not know %q in %q", f[i], l)
		}
	}
	return r
}

// judge runs the packet through the chain.
func (tb *table) judge(chainName string, p packet) verdict {
	ch := tb.chains[chainName]
	for _, r := range ch.rules {
		ok := true
		for _, m := range r.matches {
			ok = ok && m(p)
		}
		if ok {
			return verdict{r.accept, r.comment}
		}
	}
	return verdict{ch.policy == "accept", "policy " + ch.policy}
}
