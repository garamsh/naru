package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/garamsh/naru/lib/hostapi"
)

// microvmSnapshot is naructl microvm snapshot <microvm> --name <snapshot>: it has the host take a
// snapshot of a microVM and prints it.
func microvmSnapshot(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm snapshot", stderr)
	name := c.fs.String("name", "", "the name the snapshot takes")
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	if err := require(c.fs, "name"); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostCallTimeout)
	defer cancel()
	s, err := cl.SnapshotMicroVM(ctx, rest[0], hostapi.SnapshotRequest{Name: *name})
	if err != nil {
		return err
	}
	return printJSON(stdout, s)
}

// microvmFence runs the subcommands of naructl microvm fence.
func microvmFence(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("microvm fence needs get or renew")
	}
	switch args[0] {
	case "get":
		return fenceGet(args[1:], stdout, stderr)
	case "renew":
		return fenceRenew(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "microvm fence "+args[0])
}

func fenceGet(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm fence get", stderr)
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	f, err := cl.Fence(ctx, rest[0])
	if err != nil {
		return err
	}
	return printJSON(stdout, f)
}

func fenceRenew(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm fence renew", stderr)
	length := c.fs.Uint64("length", 0, "the fence's length in seconds: its deadline becomes now plus this")
	renewal := c.fs.Uint64("renewal", 0, "the renewal number, which must be above the last one applied")
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	if err := require(c.fs, "length", "renewal"); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	f, err := cl.RenewFence(ctx, rest[0], hostapi.FenceRequest{LengthSeconds: *length, Renewal: *renewal})
	if err != nil {
		return err
	}
	return printJSON(stdout, f)
}

// snapshotCmd runs the subcommands of naructl snapshot.
func snapshotCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("snapshot needs list, get, delete or resume")
	}
	switch args[0] {
	case "list":
		return snapshotList(args[1:], stdout, stderr)
	case "get":
		return snapshotGet(args[1:], stdout, stderr)
	case "delete":
		return snapshotDelete(args[1:], stdout, stderr)
	case "resume":
		return snapshotResume(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "snapshot "+args[0])
}

func snapshotList(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("snapshot list", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	all, err := cl.Snapshots(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tMICROVM\tIMAGE")
	for _, s := range all {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, s.MicroVM, strings.TrimSpace(s.ImageDigest))
	}
	return w.Flush()
}

func snapshotGet(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("snapshot get", stderr)
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	s, err := cl.Snapshot(ctx, rest[0])
	if err != nil {
		return err
	}
	return printJSON(stdout, s)
}

func snapshotDelete(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("snapshot delete", stderr)
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	if err := cl.DeleteSnapshot(ctx, rest[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "deleted snapshot %s\n", rest[0])
	return nil
}

// snapshotResume is naructl snapshot resume <snapshot> --name <microvm>: it has the host make a
// microVM of the snapshot and prints it.
func snapshotResume(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("snapshot resume", stderr)
	name := c.fs.String("name", "", "the new microVM's name, a DNS label")
	network := c.fs.String("network", "", "the declared network the microVM is created on")
	podPrefix := c.fs.String("pod-prefix", "", "an IPv4 prefix of /16 to /32 routed to the microVM")
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	if err := require(c.fs, "name"); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostCallTimeout)
	defer cancel()
	m, err := cl.ResumeSnapshot(ctx, rest[0], hostapi.ResumeRequest{Name: *name, Network: *network, PodPrefix: *podPrefix})
	if err != nil {
		return err
	}
	return printJSON(stdout, m)
}
