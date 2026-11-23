package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/orchapi"
)

// addressPoolFile is an address pool's file: its name and the spec orch keeps.
type addressPoolFile struct {
	Name string `json:"name"`
	orchapi.AddressPoolSpec
}

// readStrictFile reads a YAML or JSON file into v, refusing a field v does not have.
func readStrictFile(path, what string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the %s file: %w", what, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// readAddressPool reads a pool from a YAML or JSON file, refusing a field a pool does not have.
func readAddressPool(path string) (string, orchapi.AddressPoolSpec, error) {
	var f addressPoolFile
	if err := readStrictFile(path, "address pool", &f); err != nil {
		return "", orchapi.AddressPoolSpec{}, err
	}
	if f.Name == "" {
		return "", orchapi.AddressPoolSpec{}, fmt.Errorf("%s: the address pool has no name", path)
	}
	return f.Name, f.AddressPoolSpec, nil
}

func addressPoolApply(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	file := o.fs.StringP("file", "f", "", "the pool's file, YAML or JSON: name, cidrs (IPv4 ranges the LAN's owner gave) and networks (those whose controller may draw from it)")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		if err := require(o.fs, "file"); err != nil {
			return err
		}
		name, spec, err := readAddressPool(*file)
		if err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		p, err := cl.ApplyAddressPool(ctx, name, spec)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: address pool %s applied: %s for %s\n", p.Name, strings.Join(p.CIDRs, ", "), strings.Join(p.Networks, ", "))
		return nil
	}
}

func addressPoolList(fs *pflag.FlagSet) runner {
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
		ps, err := cl.AddressPools(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "POOL\tCIDRS\tNETWORKS")
		for _, p := range ps {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, strings.Join(p.CIDRs, ","), strings.Join(p.Networks, ","))
		}
		return tw.Flush()
	}
}

func addressPoolDelete(fs *pflag.FlagSet) runner {
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
		if err := cl.DeleteAddressPool(ctx, args[0]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: address pool %s deleted\n", args[0])
		return nil
	}
}

func exposureList(fs *pflag.FlagSet) runner {
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
		es, err := cl.Exposures(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NETWORK\tEXPOSURE\tOWNER\tADDRESS\tHOST\tPHASE")
		for _, e := range es {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Network, e.ID, e.Owner, e.Address, e.Host, e.Phase)
		}
		return tw.Flush()
	}
}

func exposureDelete(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 2); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.DeleteNetworkExposure(ctx, args[0], args[1]); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: exposure %s of network %s is being released\n", args[1], args[0])
		return nil
	}
}

// exposureFile is an operator's exposure file: its id and the spec orch keeps, whose network it
// names.
type exposureFile struct {
	ID string `json:"id"`
	orchapi.ExposureSpec
}

func exposureApply(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	file := o.fs.StringP("file", "f", "", "the exposure's file, YAML or JSON: id, network, pool, requestedAddress, ports (protocol, port, backendPort), backends and optionally proxyProtocol")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		if err := require(o.fs, "file"); err != nil {
			return err
		}
		var f exposureFile
		if err := readStrictFile(*file, "exposure", &f); err != nil {
			return err
		}
		if f.ID == "" || f.Network == "" {
			return fmt.Errorf("%s: the exposure needs an id and a network", *file)
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		e, err := cl.ApplyNetworkExposure(ctx, f.Network, f.ID, f.ExposureSpec)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: exposure %s of network %s applied: address %s, %s\n", e.ID, e.Network, e.Address, e.Phase)
		return nil
	}
}

func exposureGet(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 2); err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		e, err := cl.NetworkExposure(ctx, args[0], args[1])
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
