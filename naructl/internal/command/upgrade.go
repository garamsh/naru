package command

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/garamsh/naru/naructl/internal/upgrade"
)

// upgradeTimeout bounds the whole of an upgrade call: the bundle is tens of megabytes.
const upgradeTimeout = 10 * time.Minute

// upgradeCmd sends a bundle to a Node, or with --status reports the Node's slot and last upgrade.
// It is not named upgrade because the package of that name is imported here.
func upgradeCmd(args []string, stdout, stderr io.Writer) error {
	c := newCall("upgrade", stderr)
	bundle := c.fs.String("bundle", "", "the upgrade bundle, naru-host-upgrade-<version>.tar")
	sums := c.fs.String("sums", "", "the release's SHA256SUMS, which lists the bundle")
	status := c.fs.Bool("status", false, "print the Node's slot and the outcome of its last upgrade, and send nothing")
	if _, err := c.parse(args, 0); err != nil {
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
	cl, err := c.dial()
	if err != nil {
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
