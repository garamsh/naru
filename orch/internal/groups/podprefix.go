package groups

import (
	"fmt"
	"net/netip"

	"github.com/garamsh/naru/orch/internal/prefixes"
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

// take returns the first prefix of the given length inside the network's prefix that overlaps none
// held, and records it as held.
func (p *podPrefixes) take(n Network, bits int) (netip.Prefix, error) {
	space, err := netip.ParsePrefix(n.Prefix)
	if err != nil || !space.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("network %s has prefix %q, which is not an IPv4 prefix", n.Name, n.Prefix)
	}
	c, err := prefixes.Allocate(space, bits, p.held[n.Name])
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("network %s: %w", n.Name, err)
	}
	p.held[n.Name] = append(p.held[n.Name], c)
	return c, nil
}
