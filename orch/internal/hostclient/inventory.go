package hostclient

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Inventory lists the admitted hosts.
type Inventory interface {
	Hosts(ctx context.Context) ([]orchapi.Host, error)
}

// reached are the admitted hosts orch reaches: every one but an abandoned host, which is in no
// peer list, no exposure's or group's choice and no guest call (ADR 0109 item 6). Only the Machine
// reconciler sees an abandoned host, to mark its Machines lost.
func reached(ctx context.Context, inv Inventory) ([]orchapi.Host, error) {
	all, err := inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, h := range all {
		if h.Abandonment == nil {
			out = append(out, h)
		}
	}
	return out, nil
}
