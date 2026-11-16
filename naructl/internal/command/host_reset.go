package command

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/hostapi"
)

// hostReset is naructl reset: the host API's reset call, which clears the host's data disk by scope
// and reboots it (ADR 0082). It also reaches a host in maintenance mode.
func hostReset(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	scope := fs.String("scope", "", "what to remove: store (the stored configuration, so the host runs its base again), data (everything on the data disk but the stored configuration) or all (blank the data disk, so the host formats it again)")
	confirm := fs.String("confirm", "", "the host's ID, which naructl host or a host in maintenance mode reports as hostId: a reset aimed at another host is refused")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 0); err != nil {
			return err
		}
		if err := require(fs, "scope", "confirm"); err != nil {
			return err
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		out, err := cl.Reset(ctx, hostapi.ResetRequest{Scope: *scope, Confirm: *confirm})
		var apiErr *hostapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == hostapi.CodeRevokedPair {
			return fmt.Errorf("%w: rotate the pair the host's configuration names and update the seed before a reset of scope %s", err, *scope)
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: the host accepted the reset of scope %s and is rebooting\n", out.Scope)
		return nil
	}
}
