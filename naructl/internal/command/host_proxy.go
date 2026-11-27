package command

import (
	"fmt"
	"io"

	"github.com/spf13/pflag"
)

// hostProxy is host grant-proxy or host revoke-proxy: it asks orch to give the host the proxy
// role, so its proxy takes the routing table and its peers let it reach their Machines, or to take
// the role back (ADR 0119). verb is "grant" or "revoke".
func hostProxy(verb string) binder {
	return func(fs *pflag.FlagSet) runner {
		o := newOrchFlags(fs)
		return func(args []string, stdout, _ io.Writer) error {
			if err := o.check(args, 1); err != nil {
				return err
			}
			_, cl, err := o.connect()
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()
			act := cl.GrantProxy
			if verb == "revoke" {
				act = cl.RevokeProxy
			}
			h, err := act(ctx, args[0])
			if err != nil {
				return err
			}
			if h.Proxy {
				_, _ = fmt.Fprintf(stdout, "naructl: host %s holds the proxy role\n", h.ID)
			} else {
				_, _ = fmt.Fprintf(stdout, "naructl: host %s holds no proxy role\n", h.ID)
			}
			return nil
		}
	}
}
