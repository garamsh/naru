package orchd

import (
	"context"
	"net/netip"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// addressSpace is inventory.AddressSpace over the networks and the address pools orch keeps.
type addressSpace struct {
	networks interface {
		List(ctx context.Context) ([]orchapi.Network, error)
	}
	pools interface {
		ListPools(ctx context.Context) ([]orchapi.AddressPool, error)
	}
}

// Claimed returns each network's prefix and each pool's CIDRs.
func (a addressSpace) Claimed(ctx context.Context) ([]inventory.Claim, error) {
	nets, err := a.networks.List(ctx)
	if err != nil {
		return nil, err
	}
	pools, err := a.pools.ListPools(ctx)
	if err != nil {
		return nil, err
	}
	var out []inventory.Claim
	for _, n := range nets {
		if p, err := netip.ParsePrefix(n.Prefix); err == nil {
			out = append(out, inventory.Claim{Prefix: p, By: "network " + n.Name})
		}
	}
	for _, pool := range pools {
		for _, c := range pool.CIDRs {
			if p, err := netip.ParsePrefix(c); err == nil {
				out = append(out, inventory.Claim{Prefix: p, By: "address pool " + pool.Name})
			}
		}
	}
	return out, nil
}
