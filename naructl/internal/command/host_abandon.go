package command

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/spf13/pflag"
)

// hostAbandon declares a fenced host gone for good: orch records it, forwards the operator's own
// signed revocation of the host to the signer, and marks the host's Machines lost (ADR 0109 item 6).
func hostAbandon(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	fenced := o.fs.String("fenced", "", "how the host was fenced, so it runs nothing: powered off, its disks pulled")
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		id := args[0]
		if strings.TrimSpace(*fenced) == "" {
			return fmt.Errorf("--fenced is required: say how the host was fenced; a host is abandoned only once it runs nothing")
		}
		person, cl, err := o.connect()
		if err != nil {
			return err
		}
		signed, err := identity.SignRequest(identity.ActionRevokeHost, identity.HostSerialsRequest{HostID: id}, time.Now().Add(operatorRequestTTL), person.Key, person.Cert)
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		h, err := cl.AbandonHost(ctx, id, orchapi.AbandonRequest{Fenced: *fenced, RevokeHost: signed})
		if err != nil {
			return fmt.Errorf("%w (if orch recorded the abandonment, naructl host get %s shows it, and host abandon again retries the revocation)", err, id)
		}
		_, _ = fmt.Fprintf(stdout, "naructl: host %s is abandoned; %s\n", h.ID, revocationText(h.Abandonment))
		return nil
	}
}

// revocationText is where the revocation of an abandoned host stands.
func revocationText(a *orchapi.HostAbandonment) string {
	switch {
	case a == nil:
		return "orch recorded no abandonment"
	case a.Revocation.Revoked():
		return "its certificates are revoked (" + strings.Join(a.Revocation.Serials, ", ") + ")"
	default:
		return "its revocation is pending: " + a.Revocation.Error + "; host abandon again retries it"
	}
}

// hostGet prints one admitted host as orch records it, with its abandonment.
func hostGet(fs *pflag.FlagSet) runner {
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
		hs, err := cl.Hosts(ctx)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.ID == args[0] {
				return printJSON(stdout, h)
			}
		}
		return fmt.Errorf("no admitted host %q", args[0])
	}
}
