package command

import (
	"cmp"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

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
	return func(args []string, stdout, _ io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		caller, name, err := machineRef(args[0])
		if err != nil {
			return err
		}
		if *cancelMove && (*region != "" || len(labels) > 0) {
			return fmt.Errorf("--cancel takes no --region or --label")
		}
		var req orchapi.MoveRequest
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
		default:
			_, _ = fmt.Fprintf(stdout, "naructl: Machine %s/%s moves from %s, %s\n", m.Caller, m.Name, m.Move.Source, m.Move.Step)
		}
		return nil
	}
}
