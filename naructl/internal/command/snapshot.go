package command

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/hostapi"
)

// microvmSnapshot is naructl microvm snapshot <microvm> --name <snapshot>: it has the host take a
// snapshot of a microVM and prints it.
func microvmSnapshot(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	name := c.fs.String("name", "", "the name the snapshot takes")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
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
		s, err := cl.SnapshotMicroVM(ctx, args[0], hostapi.SnapshotRequest{Name: *name})
		if err != nil {
			return err
		}
		return printJSON(stdout, s)
	}
}

func fenceGet(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
			return err
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		f, err := cl.Fence(ctx, args[0])
		if err != nil {
			return err
		}
		return printJSON(stdout, f)
	}
}

func fenceRenew(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	length := c.fs.Uint64("length", 0, "the fence's length in seconds: its deadline becomes now plus this")
	renewal := c.fs.Uint64("renewal", 0, "the renewal number, which must be above the last one applied")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
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
		f, err := cl.RenewFence(ctx, args[0], hostapi.FenceRequest{LengthSeconds: *length, Renewal: *renewal})
		if err != nil {
			return err
		}
		return printJSON(stdout, f)
	}
}

func snapshotList(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 0); err != nil {
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
}

func snapshotGet(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
			return err
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		s, err := cl.Snapshot(ctx, args[0])
		if err != nil {
			return err
		}
		return printJSON(stdout, s)
	}
}

func snapshotDelete(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
			return err
		}
		cl, err := c.dial()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.DeleteSnapshot(ctx, args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "deleted snapshot %s\n", args[0])
		return nil
	}
}

// snapshotResume is naructl snapshot resume <snapshot> --name <microvm>: it has the host make a
// microVM of the snapshot and prints it.
func snapshotResume(fs *pflag.FlagSet) runner {
	c := newHostCall(fs)
	name := c.fs.String("name", "", "the new microVM's name, a DNS label")
	network := c.fs.String("network", "", "the declared network the microVM is created on")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
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
		m, err := cl.ResumeSnapshot(ctx, args[0], hostapi.ResumeRequest{Name: *name, Network: *network})
		if err != nil {
			return err
		}
		return printJSON(stdout, m)
	}
}
