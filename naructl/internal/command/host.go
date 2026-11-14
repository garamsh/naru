package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/garamsh/naru/naructl/internal/client"
	"github.com/garamsh/naru/naructl/internal/layout"
	"github.com/garamsh/naru/naructl/internal/pki"
)

// hostCallTimeout bounds a microVM create, which pulls an image and boots the Machine.
const hostCallTimeout = callTimeout * 10

func genHostCA(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen host-ca", stderr)
	caDir := fs.String("ca-dir", "", "the cluster's directory, which receives host/ca.pem and host/ca.key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca-dir"); err != nil {
		return err
	}
	dir := filepath.Join(*caDir, pki.HostDir)
	for _, name := range []string{pki.CertFile, pki.KeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%s already holds a host authority; a new one would replace it", dir)
		}
	}
	ca, err := pki.New("naru-host-ca")
	if err != nil {
		return err
	}
	if err := ca.Write(dir); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s and %s; keep %s in your secret store\n", filepath.Join(dir, pki.CertFile), filepath.Join(dir, pki.KeyFile), pki.KeyFile)
	return nil
}

func genHostClient(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen host-client", stderr)
	var endpoints list
	caDir := fs.String("ca-dir", "", "the cluster's directory that holds host/ca.pem and host/ca.key")
	role := fs.String("role", "", "provisioner (reads, creates and destroys microVMs) or reader (reads)")
	cn := fs.String("cn", "", "the certificate's CN, the caller's name in a host's request log (default naru-host-<role>)")
	out := fs.String("out", "", "the client file to write (0600: it carries a key; a file already there is replaced)")
	fs.Var(&endpoints, "endpoint", "a host's address, an IP address with or without a port (default "+client.HostPort+"), repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca-dir", "role", "endpoint", "out"); err != nil {
		return err
	}
	if *cn == "" {
		*cn = "naru-host-" + *role
	}
	authority, err := pki.Load(filepath.Join(*caDir, pki.HostDir))
	if err != nil {
		return fmt.Errorf("the host authority (run gen host-ca first): %w", err)
	}
	f, err := client.IssueHost(authority, *role, *cn, endpoints)
	if err != nil {
		return err
	}
	if err := f.Write(*out); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s for role %s\n", *out, *role)
	return nil
}

// loadHost reads the host authority of caDir and returns the Host a Node serves with: the region,
// and the labels as key=value pairs.
func loadHost(caDir, region string, labels list) (*layout.Host, error) {
	ca, err := pki.Load(filepath.Join(caDir, pki.HostDir))
	if err != nil {
		return nil, fmt.Errorf("the host authority (run gen host-ca first): %w", err)
	}
	h := &layout.Host{CA: ca, Region: region, Labels: map[string]string{}}
	for _, l := range labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--host-label %q is not <key>=<value>", l)
		}
		h.Labels[k] = v
	}
	return h, nil
}

// hostCall is a host API command: the flags every one takes, then dial and run.
type hostCall struct {
	fs           *flag.FlagSet
	config, host *string
}

func newHostCall(name string, stderr io.Writer) hostCall {
	fs := newFlags(name, stderr)
	return hostCall{
		fs:     fs,
		config: fs.String("config", "", "the client file written by gen host-client"),
		host:   fs.String("host", "", "the host's address, an IP address with or without a port (default the client file's first endpoint)"),
	}
}

func (c hostCall) parse(args []string, positional int) ([]string, error) {
	return call{fs: c.fs, config: c.config}.parse(args, positional)
}

func (c hostCall) dial() (*client.HostClient, error) {
	f, err := client.LoadFile(*c.config)
	if err != nil {
		return nil, err
	}
	return client.DialHost(f, *c.host)
}

func hostCmd(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("host", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	h, err := cl.Host(ctx)
	if err != nil {
		return err
	}
	return printJSON(stdout, h)
}

func printJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// microvm runs the subcommands of naructl microvm.
func microvm(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("microvm needs create, get, list, stop, start or delete")
	}
	switch args[0] {
	case "create":
		return microvmCreate(args[1:], stdout, stderr)
	case "get":
		return microvmGet(args[1:], stdout, stderr)
	case "list":
		return microvmList(args[1:], stdout, stderr)
	case "stop":
		return microvmLifecycle("stop", args[1:], stdout, stderr)
	case "start":
		return microvmLifecycle("start", args[1:], stdout, stderr)
	case "delete":
		return microvmDelete(args[1:], stdout, stderr)
	}
	return fmt.Errorf("unknown command %q", "microvm "+args[0])
}

func microvmCreate(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm create", stderr)
	var disks list
	var (
		file   = c.fs.String("f", "", "the whole request as a JSON file; with it no other field is given")
		name   = c.fs.String("name", "", "the microVM's name, a DNS label")
		image  = c.fs.String("image", "", "repository@sha256:<digest>")
		cpu    = c.fs.Uint("cpu", 1, "virtual CPUs")
		memory = c.fs.Uint64("memory", 256<<20, "memory in bytes")
		root   = c.fs.Uint64("root-disk", 1<<30, "root disk in bytes")

		bootSet   = c.fs.String("boot-set", "", "the OS form: the boot set the Node offers, in place of --image")
		cmdline   = c.fs.String("cmdline", "", "the OS form: kernel command line added to the boot set's")
		seed      = c.fs.String("seed", "", "the OS form: a file of the NoCloud user-data")
		dataDisk  = c.fs.Uint64("data-disk", 0, "the OS form: the data disk in bytes")
		podPrefix = c.fs.String("pod-prefix", "", "the OS form: an IPv4 prefix of /16 to /32 routed to the microVM")
	)
	c.fs.Var(&disks, "disk", "<name>=<bytes>, with ,ro for a read-only disk, repeatable")
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	var spec client.MicroVMSpec
	if *file != "" {
		set := map[string]bool{}
		c.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		for _, f := range []string{"name", "image", "cpu", "memory", "root-disk", "disk", "boot-set", "cmdline", "seed", "data-disk", "pod-prefix"} {
			if set[f] {
				return fmt.Errorf("-f carries the whole request, so --%s has no effect with it", f)
			}
		}
		b, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&spec); err != nil {
			return errors.New(*file + " is not a microVM request: " + err.Error())
		}
	} else if osForm := *bootSet != "" || *cmdline != "" || *seed != "" || *dataDisk != 0 || *podPrefix != ""; osForm {
		set := map[string]bool{}
		c.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		for _, f := range []string{"image", "root-disk", "disk"} {
			if set[f] {
				return fmt.Errorf("--%s is the image form's: a microVM has an image or a boot set, not both", f)
			}
		}
		if err := require(c.fs, "name", "boot-set", "seed", "data-disk"); err != nil {
			return err
		}
		userData, err := os.ReadFile(*seed)
		if err != nil {
			return err
		}
		spec = client.MicroVMSpec{ //nolint:gosec // flags, bounded by the API's own validation
			Name: *name, CPU: uint32(*cpu), Memory: *memory, BootSet: *bootSet, Cmdline: *cmdline,
			Seed: userData, DataDisk: *dataDisk, PodPrefix: *podPrefix,
		}
	} else {
		if err := require(c.fs, "name", "image"); err != nil {
			return err
		}
		spec = client.MicroVMSpec{Name: *name, Image: *image, CPU: uint32(*cpu), Memory: *memory, RootDiskBytes: *root} //nolint:gosec // a flag, bounded by the API's own validation
		for _, d := range disks {
			disk, err := parseDisk(d)
			if err != nil {
				return fmt.Errorf("--disk: %w", err)
			}
			spec.Disks = append(spec.Disks, disk)
		}
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostCallTimeout)
	defer cancel()
	m, created, err := cl.Create(ctx, spec)
	if err != nil {
		return err
	}
	if created {
		_, _ = fmt.Fprintf(stdout, "created %s\n", m.Name)
	} else {
		_, _ = fmt.Fprintf(stdout, "%s exists with this request\n", m.Name)
	}
	return printJSON(stdout, m)
}

func parseDisk(s string) (client.Disk, error) {
	s, ro := strings.CutSuffix(s, ",ro")
	name, size, ok := strings.Cut(s, "=")
	n, err := strconv.ParseUint(size, 10, 64)
	if !ok || err != nil || name == "" {
		return client.Disk{}, fmt.Errorf("%q is not <name>=<bytes>[,ro]", s)
	}
	return client.Disk{Name: name, SizeBytes: n, ReadOnly: ro}, nil
}

func microvmGet(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm get", stderr)
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
	m, err := cl.Get(ctx, rest[0])
	if err != nil {
		return err
	}
	return printJSON(stdout, m)
}

func microvmList(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm list", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	ms, err := cl.List(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSTATE\tADDRESSES\tCPU\tMEMORY")
	for _, m := range ms {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\n", m.Name, m.State, strings.Join(m.Addresses, ","), m.CPU, m.Memory)
	}
	return w.Flush()
}

// microvmLifecycle is stop or start: it asks the host and prints the microVM as it is after.
func microvmLifecycle(verb string, args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm "+verb, stderr)
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostCallTimeout)
	defer cancel()
	act := cl.Stop
	if verb == "start" {
		act = cl.Start
	}
	m, err := act(ctx, rest[0])
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s %s: %s\n", verb, m.Name, m.State)
	return printJSON(stdout, m)
}

func microvmDelete(args []string, stdout, stderr io.Writer) error {
	c := newHostCall("microvm delete", stderr)
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), hostCallTimeout)
	defer cancel()
	if err := cl.Delete(ctx, rest[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "deleted %s\n", rest[0])
	return nil
}
