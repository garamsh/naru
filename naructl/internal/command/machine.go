package command

import (
	"cmp"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/spf13/pflag"
)

// machineList lists every caller's Machines: where each is bound, and the step of its move.
func machineList(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		ms, err := cl.Machines(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "CALLER\tNAME\tPHASE\tHOST\tEPOCH\tMOVE")
		for _, m := range ms {
			host, epoch := "-", "-"
			if b := m.Binding; b != nil {
				host, epoch = b.Host, strconv.FormatUint(b.Epoch, 10)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.Caller, m.Name, m.Status.Phase, host, epoch, moveText(m.Move))
		}
		return tw.Flush()
	}
}

// moveText is a move as one column: its step and where it goes, or a dash.
func moveText(m *orchapi.MachineMove) string {
	if m == nil {
		return "-"
	}
	s := m.Step + " " + m.Source + "->" + cmp.Or(m.Target, "?")
	if m.Cancelled {
		s += " (cancelled)"
	}
	return s
}

// machineRef splits <caller>/<name>, the Machine an operator names.
func machineRef(arg string) (caller, name string, err error) {
	caller, name, ok := strings.Cut(arg, "/")
	if !ok || caller == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%q is not <caller>/<name>", arg)
	}
	return caller, name, nil
}

// machineGet prints one caller's Machine as the operator API shows it.
func machineGet(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		caller, name, err := machineRef(args[0])
		if err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		m, err := cl.Machine(ctx, caller, name)
		if err != nil {
			return err
		}
		return printJSON(stdout, m)
	}
}

// machineMove asks orch to move a caller's Machine to another host, within its shape narrowed by
// --region and --label, or with --cancel to abandon the move before its commit.
func machineMove(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	var labels list
	region := o.fs.String("region", "", "the region the Machine moves to, which its shape must allow")
	o.fs.Var(&labels, "label", "a label the target host must carry, <key>=<value>, repeatable")
	cancelMove := o.fs.Bool("cancel", false, "abandon the move in progress, which a move past its commit refuses")
	abandonVolumes := o.fs.Bool("abandon-volumes", false, "move a lost Machine, whose host was abandoned, with new, empty volumes")
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		caller, name, err := machineRef(args[0])
		if err != nil {
			return err
		}
		if *cancelMove && (*region != "" || len(labels) > 0 || *abandonVolumes) {
			return fmt.Errorf("--cancel takes no --region, --label or --abandon-volumes")
		}
		req := orchapi.MoveRequest{AbandonVolumes: *abandonVolumes}
		if *region != "" || len(labels) > 0 {
			l, err := attributes(labels)
			if err != nil {
				return err
			}
			if len(l) == 0 {
				l = nil
			}
			req.Shape = &orchapi.MoveShape{Region: *region, Labels: l}
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		var m orchapi.Machine
		if *cancelMove {
			m, err = cl.CancelMove(ctx, caller, name)
		} else {
			m, err = cl.MoveMachine(ctx, caller, name, req)
		}
		if err != nil {
			return err
		}
		switch {
		case m.Move == nil:
			_, _ = fmt.Fprintf(stdout, "naructl: Machine %s/%s has no move\n", m.Caller, m.Name)
		case m.Move.Cancelled:
			_, _ = fmt.Fprintf(stdout, "naructl: the move of Machine %s/%s is cancelled at %s\n", m.Caller, m.Name, m.Move.Step)
		case m.Move.AbandonVolumes:
			_, _ = fmt.Fprintf(stdout, "naructl: Machine %s/%s moves from %s with new volumes, %s; its old ones stay listed in machine abandoned-volumes list\n", m.Caller, m.Name, m.Move.Source, m.Move.Step)
		default:
			_, _ = fmt.Fprintf(stdout, "naructl: Machine %s/%s moves from %s, %s\n", m.Caller, m.Name, m.Move.Source, m.Move.Step)
		}
		return nil
	}
}

// abandonedVolumesList lists the volumes left behind, by the Machine and epoch each belonged to:
// a placement on an abandoned host (bound), or a move's source copy whose target was abandoned
// after the commit (source).
func abandonedVolumesList(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		vs, err := cl.AbandonedVolumes(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "CALLER\tNAME\tHOST\tEPOCH\tROLE\tVOLUMES\tAT")
		for _, v := range vs {
			role := v.Role
			if v.Deleting {
				role += " (deleting)"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", v.Caller, v.Name, v.Host, v.Epoch, role, cmp.Or(strings.Join(v.Volumes, ","), "-"), v.At.Format(time.RFC3339))
		}
		return tw.Flush()
	}
}

// abandonedVolumesDelete deletes the volumes of one Machine's epoch left behind, once the operator
// has nothing left to recover from them: orch forgets those on an abandoned host, and deletes a
// source copy on its host first.
func abandonedVolumesDelete(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	host := o.fs.String("host", "", "the abandoned host the volumes were on")
	epoch := o.fs.Uint64("epoch", 0, "the epoch of the Machine the volumes belonged to")
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		caller, name, err := machineRef(args[0])
		if err != nil {
			return err
		}
		if err := require(o.fs, "host", "epoch"); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.DeleteAbandonedVolumes(ctx, caller, name, *host, *epoch); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: deleted the volumes of Machine %s/%s epoch %d on host %s; a source copy is deleted on its host before it leaves the list\n", caller, name, *epoch, *host)
		return nil
	}
}
