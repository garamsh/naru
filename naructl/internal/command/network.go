package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/orchapi"
)

// networkCmd runs naructl network apply, get, list and delete: the virtual networks orch declares
// to every host.
func networkCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("network needs apply, get, list or delete")
	}
	switch args[0] {
	case "apply":
		return networkApply(args[1:], stdout, stderr)
	case "get":
		return networkGet(args[1:], stdout, stderr)
	case "list":
		return networkList(args[1:], stdout, stderr)
	case "delete":
		return networkDelete(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "network "+args[0])
}

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

func networkApply(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("network apply", stderr)
	file := o.fs.String("f", "", "the network's file, YAML or JSON: name, prefix and optionally apiEndpoint")
	if _, err := o.parse(args, 0); err != nil {
		return err
	}
	if err := require(o.fs, "f"); err != nil {
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

func networkGet(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("network get", stderr)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("network get needs the network's name first")
	}
	name := args[0]
	if _, err := o.parse(args[1:], 0); err != nil {
		return err
	}
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

func networkList(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("network list", stderr)
	if _, err := o.parse(args, 0); err != nil {
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

func networkDelete(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("network delete", stderr)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("network delete needs the network's name first")
	}
	name := args[0]
	if _, err := o.parse(args[1:], 0); err != nil {
		return err
	}
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
