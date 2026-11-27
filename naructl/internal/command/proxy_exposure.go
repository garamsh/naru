package command

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/orchapi"
)

// proxyExposureFile is a proxy exposure's file: its id and the spec orch keeps.
type proxyExposureFile struct {
	ID string `json:"id"`
	orchapi.ProxyExposureSpec
}

func ports(ps []int) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

func proxyExposureApply(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	file := o.fs.StringP("file", "f", "", "the proxy exposure's file, YAML or JSON: id, pool (any address pool), requestedAddress, and ports (443, and 80 for the redirect to https)")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		if err := require(o.fs, "file"); err != nil {
			return err
		}
		var f proxyExposureFile
		if err := readStrictFile(*file, "proxy exposure", &f); err != nil {
			return err
		}
		if f.ID == "" {
			return fmt.Errorf("%s: the proxy exposure has no id", *file)
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		e, err := cl.ApplyProxyExposure(ctx, f.ID, f.ProxyExposureSpec)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: proxy exposure %s applied: address %s, ports %s, %s\n", e.ID, e.Address, ports(e.Ports), e.Phase)
		return nil
	}
}

func proxyExposureGet(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		e, err := cl.ProxyExposure(ctx, args[0])
		if err != nil {
			return err
		}
		out, err := json.MarshalIndent(e, "", "  ")
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "%s\n", out)
		return nil
	}
}

func proxyExposureList(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		es, err := cl.ProxyExposures(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "PROXY EXPOSURE\tPOOL\tADDRESS\tPORTS\tHOST\tPHASE")
		for _, e := range es {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, e.Pool, e.Address, ports(e.Ports), e.Host, e.Phase)
		}
		return tw.Flush()
	}
}

func proxyExposureDelete(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.DeleteProxyExposure(ctx, args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: proxy exposure %s is being released\n", args[0])
		return nil
	}
}
