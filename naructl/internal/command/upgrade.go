package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/naructl/internal/client"
	"github.com/garamsh/naru/naructl/internal/upgrade"
)

// upgradeTimeout bounds the whole of an upgrade call: the bundle is tens of megabytes.
const upgradeTimeout = 10 * time.Minute

// upgradeCmd sends a bundle to a Node, or with --status reports the Node's slot and last upgrade.
// It is not named upgrade because the package of that name is imported here.
func upgradeCmd(fs *pflag.FlagSet) runner {
	c := newCall(fs)
	bundle := c.fs.String("bundle", "", "the upgrade bundle, naru-host-upgrade-<version>.tar")
	sums := c.fs.String("sums", "", "the release's SHA256SUMS, which lists the bundle")
	status := c.fs.Bool("status", false, "print the Node's slot and the outcome of its last upgrade, and send nothing")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 0); err != nil {
			return err
		}
		if *status {
			if *bundle != "" || *sums != "" {
				return fmt.Errorf("--status takes neither --bundle nor --sums")
			}
			return upgradeStatus(c, stdout)
		}
		if err := require(c.fs, "bundle", "sums"); err != nil {
			return err
		}
		// The bundle is checked against the sums file before the Node is dialled, and a bundle that
		// does not match is refused without a byte sent.
		f, err := os.Open(*sums)
		if err != nil {
			return err
		}
		want, err := upgrade.SumFor(f, filepath.Base(*bundle))
		_ = f.Close()
		if err != nil {
			return err
		}
		if err := upgrade.Check(*bundle, want); err != nil {
			return err
		}
		version, err := upgrade.BundleVersion(*bundle)
		if err != nil {
			return err
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		if err := requireExtensions(cl, version); err != nil {
			return err
		}
		b, err := os.Open(*bundle)
		if err != nil {
			return err
		}
		defer func() { _ = b.Close() }()
		info, err := b.Stat()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), upgradeTimeout)
		defer cancel()
		actions, err := cl.Upgrade(ctx, want, b, info.Size())
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stdout, "the Node accepted the upgrade:")
		for _, a := range actions {
			_, _ = fmt.Fprintf(stdout, "  %s %s\n", a.Action, a.Target)
		}
		_, _ = fmt.Fprintln(stdout, "the Node reboots into the new kernel and keeps it only when it is healthy; naructl version shows the version it runs and naructl upgrade --status how the upgrade ended")
		return nil
	}
}

// requireExtensions refuses an upgrade to version when the Node's configuration lists a system
// extension of which the Node stores no image built for version: its trial boot would refuse
// (ADR 0055) and roll back.
func requireExtensions(cl *client.Client, version string) error {
	ctx, cancel := withTimeout()
	defer cancel()
	cfg, err := cl.Config(ctx)
	if err != nil {
		return err
	}
	var doc struct {
		Spec struct {
			Extensions []upgrade.Entry `json:"extensions"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(cfg.Config, &doc); err != nil {
		return fmt.Errorf("read spec.extensions of the Node's configuration: %w", err)
	}
	if len(doc.Spec.Extensions) == 0 {
		return nil
	}
	have, err := cl.Extensions(ctx)
	if err != nil {
		return err
	}
	stored := make([]upgrade.Image, len(have))
	for i, e := range have {
		stored[i] = upgrade.Image{Name: e.Name, SHA256: e.SHA256, Version: e.Version}
	}
	missing := upgrade.MissingExtensions(doc.Spec.Extensions, stored, version)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("the Node would refuse its boot on %s: no stored image of %s is built for it and named in spec.extensions. "+
		"Deliver the release's image with `naructl extension add`, add its entry beside the old one with `naructl apply --mode staged`, "+
		"post the bundle again, and remove the old entry and image once the new slot is promoted (ADR 0055, Consequences)",
		version, strings.Join(missing, ", "))
}

func upgradeStatus(c call, stdout io.Writer) error {
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	s, err := cl.Version(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "version  %s\nboot ID  %s\nslot     %s\n", s.Version, s.BootID, s.Slot)
	if o := s.LastUpgrade; o == nil {
		_, _ = fmt.Fprintln(stdout, "upgrade  none has ended")
	} else {
		_, _ = fmt.Fprintf(stdout, "upgrade  %s -> %s, %s at %s\n", o.From, o.To, o.Result, o.Time)
		if o.Reason != "" {
			_, _ = fmt.Fprintf(stdout, "reason   %s\n", o.Reason)
		}
	}
	return nil
}
