package node

import (
	"context"
	"errors"

	rt "github.com/garamsh/naru/tooling/qemu/internal/route"
)

// RouteTo opens a listener on the test host's loopback that reaches target, "address:port" of a
// microVM on node-1's Machine prefix, through node-1's naru-mvmroute, and returns its address
// "127.0.0.1:port" and a func that closes it. A client file, kubeconfig or curl that is given the
// returned address reaches the microVM's own service (docs/architecture/harness.md).
func (h *HostAPI) RouteTo(ctx context.Context, target string) (addr string, stop func(), err error) {
	if h == nil || h.Route == "" {
		return "", nil, errors.New("the cluster has no route to its microVMs: it was started without Host")
	}
	return rt.Forward(ctx, h.Route, target)
}
