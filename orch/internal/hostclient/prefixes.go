package hostclient

import (
	"context"
	"sync"

	"github.com/garamsh/naru/orch/internal/inventory"
)

var _ inventory.PrefixHosts = (*Client)(nil)

// MachinePrefixes returns the Machine prefixes each admitted host reports in GET /v1/host. A host
// that does not answer is left out.
func (c *Client) MachinePrefixes(ctx context.Context) (map[string][]string, error) {
	admitted, err := c.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(admitted))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, h := range admitted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			info, err := c.api(h).Host(ctx)
			if err != nil {
				return
			}
			mu.Lock()
			out[h.ID] = info.Mvmd.MachinePrefixes
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out, nil
}
