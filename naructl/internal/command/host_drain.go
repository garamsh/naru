package command

import (
	"fmt"
	"io"

	"github.com/spf13/pflag"
)

// hostDrain is host drain or host undrain: it asks orch to stop placing on the host and move its
// members off it, or to place on it again. verb is "drain" or "undrain".
func hostDrain(verb string) binder {
	return func(fs *pflag.FlagSet) runner {
		o := newOrchFlags(fs)
		return func(args []string, stdout, _ io.Writer) error {
			if err := o.check(args, 1); err != nil {
				return err
			}
			id := args[0]
			_, cl, err := o.connect()
			if err != nil {
				return err
			}
			ctx, cancel := withTimeout()
			defer cancel()
			act := cl.Drain
			if verb == "undrain" {
				act = cl.Undrain
			}
			h, err := act(ctx, id)
			if err != nil {
				return err
			}
			if h.Draining {
				_, _ = fmt.Fprintf(stdout, "naructl: host %s is draining\n", h.ID)
			} else {
				_, _ = fmt.Fprintf(stdout, "naructl: host %s is not draining\n", h.ID)
			}
			return nil
		}
	}
}
