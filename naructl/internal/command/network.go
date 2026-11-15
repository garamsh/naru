package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/orchapi"
)

// networkFile is a network's file: its name and the spec orch keeps.
type networkFile struct {
	Name string `json:"name"`
	orchapi.NetworkSpec
}

// readNetwork reads a network from a YAML or JSON file, refusing a field a network does not have.
func readNetwork(path string) (string, orchapi.NetworkSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", orchapi.NetworkSpec{}, fmt.Errorf("read the network file: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", orchapi.NetworkSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return "", orchapi.NetworkSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	var f networkFile
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return "", orchapi.NetworkSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Name == "" {
		return "", orchapi.NetworkSpec{}, fmt.Errorf("%s: the network has no name", path)
	}
	return f.Name, f.NetworkSpec, nil
}

func networkApply(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	file := o.fs.StringP("file", "f", "", "the network's file, YAML or JSON: name, prefix and optionally apiEndpoint")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 0); err != nil {
			return err
		}
		if err := require(o.fs, "file"); err != nil {
			return err
		}
		name, spec, err := readNetwork(*file)
		if err != nil {
			return err
		}
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		n, err := cl.ApplyNetwork(ctx, name, spec)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: network %s applied: %s\n", n.Name, n.Prefix)
		return nil
	}
}

func networkGet(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		name := args[0]
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		n, err := cl.Network(ctx, name)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "network %s: prefix %s", n.Name, n.Prefix)
		if n.APIEndpoint != "" {
			_, _ = fmt.Fprintf(stdout, ", API endpoint %s", n.APIEndpoint)
		}
		_, _ = fmt.Fprintln(stdout)
		return nil
	}
}

func networkList(fs *pflag.FlagSet) runner {
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
		ns, err := cl.Networks(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NETWORK\tPREFIX\tAPI ENDPOINT")
		for _, n := range ns {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", n.Name, n.Prefix, n.APIEndpoint)
		}
		return tw.Flush()
	}
}

func networkDelete(fs *pflag.FlagSet) runner {
	o := newOrchFlags(fs)
	return func(args []string, stdout, stderr io.Writer) error {
		if err := o.check(args, 1); err != nil {
			return err
		}
		name := args[0]
		_, cl, err := o.connect()
		if err != nil {
			return err
		}
		ctx, cancel := withTimeout()
		defer cancel()
		if err := cl.DeleteNetwork(ctx, name); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "naructl: network %s deleted\n", name)
		return nil
	}
}
