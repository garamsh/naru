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

// groupCmd runs naructl group apply, get, list and delete: a group of microVMs orch keeps at its
// declared count.
func groupCmd(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("group needs apply, get, list or delete")
	}
	switch args[0] {
	case "apply":
		return groupApply(args[1:], stdout, stderr)
	case "get":
		return groupGet(args[1:], stdout, stderr)
	case "list":
		return groupList(args[1:], stdout, stderr)
	case "delete":
		return groupDelete(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "group "+args[0])
}

// readGroupSpec reads a group spec from a YAML or JSON file, refusing a field the spec does not
// have.
func readGroupSpec(path string) (orchapi.GroupSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return orchapi.GroupSpec{}, fmt.Errorf("read the group file: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return orchapi.GroupSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	js, err := json.Marshal(doc)
	if err != nil {
		return orchapi.GroupSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	var spec orchapi.GroupSpec
	dec := json.NewDecoder(bytes.NewReader(js))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return orchapi.GroupSpec{}, fmt.Errorf("%s: %w", path, err)
	}
	if spec.Name == "" {
		return orchapi.GroupSpec{}, fmt.Errorf("%s: the group has no name", path)
	}
	return spec, nil
}

func groupApply(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("group apply", stderr)
	file := o.fs.String("f", "", "the group's spec, YAML or JSON")
	if _, err := o.parse(args, 0); err != nil {
		return err
	}
	if err := require(o.fs, "f"); err != nil {
		return err
	}
	spec, err := readGroupSpec(*file)
	if err != nil {
		return err
	}
	_, cl, err := o.connect()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	g, err := cl.ApplyGroup(ctx, spec)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: group %s applied: %d member(s) wanted, template %s\n", g.Spec.Name, g.Spec.Count, g.Hash)
	return nil
}

func groupGet(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("group get", stderr)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("group get needs the group's name first")
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
	g, err := cl.Group(ctx, name)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "group %s: %d wanted, region %s, template %s\n", g.Spec.Name, g.Spec.Count, g.Spec.Region, g.Hash)
	if g.Invalid != "" {
		_, _ = fmt.Fprintf(stdout, "invalid: %s\n", g.Invalid)
	}
	if !g.Observed {
		_, _ = fmt.Fprintln(stdout, "the hosts did not answer: phases are the stored ones")
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MEMBER\tHOST\tPHASE\tTEMPLATE")
	for _, m := range g.Members {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ID, m.Host, m.Phase, m.TemplateHash)
	}
	return tw.Flush()
}

func groupList(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("group list", stderr)
	if _, err := o.parse(args, 0); err != nil {
		return err
	}
	_, cl, err := o.connect()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	gs, err := cl.Groups(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "GROUP\tREGION\tWANTED\tRUNNING\tMEMBERS")
	for _, g := range gs {
		running := 0
		for _, m := range g.Members {
			if m.Phase == orchapi.PhaseRunning {
				running++
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", g.Spec.Name, g.Spec.Region, g.Spec.Count, running, len(g.Members))
	}
	return tw.Flush()
}

func groupDelete(args []string, stdout, stderr io.Writer) error {
	o := newOrchFlags("group delete", stderr)
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("group delete needs the group's name first")
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
	if err := cl.DeleteGroup(ctx, name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: group %s deleted\n", name)
	return nil
}
