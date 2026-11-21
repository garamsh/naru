package hostclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/orch/internal/groups"
)

// bootSetTimeout is how long one upload of a boot set may take; the host stops it at the same
// limit.
const bootSetTimeout = 15 * time.Minute

var _ groups.BootSetHosts = (*Client)(nil)

// AddBootSet streams set to the host hostID. A host that List did not read is not admitted as far
// as this pass goes. A read of set that fails ends the call before the host commits the set.
func (c *Client) AddBootSet(ctx context.Context, hostID string, set groups.BootSet) error {
	c.mu.Lock()
	h, ok := c.pass[hostID]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("host %s is not admitted", hostID)
	}
	ctx, cancel := context.WithTimeout(ctx, bootSetTimeout)
	defer cancel()
	kernel, initramfs, err := set.Open(ctx)
	if err != nil {
		return err
	}
	defer kernel.Close()
	defer initramfs.Close()
	if kernel == nil || initramfs == nil {
		return errors.New("boot set opened no files")
	}
	return c.api(h).AddBootSet(ctx, hostapi.BootSetRequest{
		Name: set.Name, KernelSHA256: set.KernelSHA256, InitramfsSHA256: set.InitramfsSHA256,
	}, kernel, initramfs)
}
