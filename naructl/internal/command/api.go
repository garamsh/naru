package command

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"text/tabwriter"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/client"
	"github.com/garamsh/naru/naructl/internal/pki"
)

// callTimeout bounds every call but a followed log, which runs until it is interrupted.
const callTimeout = 30 * time.Second

func genAPICA(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen api-ca", stderr)
	caDir := fs.String("ca-dir", "", "the cluster's directory, which receives api/ca.pem and api/ca.key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca-dir"); err != nil {
		return err
	}
	dir := filepath.Join(*caDir, pki.APIDir)
	for _, name := range []string{pki.CertFile, pki.KeyFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%s already holds a Node API authority; a new one would replace it", dir)
		}
	}
	ca, err := pki.New("naru-api-ca")
	if err != nil {
		return err
	}
	if err := ca.Write(dir); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s and %s; keep %s in your secret store\n", filepath.Join(dir, pki.CertFile), filepath.Join(dir, pki.KeyFile), pki.KeyFile)
	return nil
}

func genAPIClient(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen api-client", stderr)
	var endpoints list
	caDir := fs.String("ca-dir", "", "the cluster's directory that holds api/ca.pem and api/ca.key")
	role := fs.String("role", "", "admin (reads and applies) or reader (reads)")
	cn := fs.String("cn", "", "the certificate's CN, the caller's name in a Node's request log (default naru-<role>)")
	out := fs.String("out", "", "the client file to write (0600: it carries a key; a file already there is replaced)")
	fs.Var(&endpoints, "endpoint", "a Node's address, an IP address with or without a port, repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca-dir", "role", "endpoint", "out"); err != nil {
		return err
	}
	if *cn == "" {
		*cn = "naru-" + *role
	}
	authority, err := pki.Load(filepath.Join(*caDir, pki.APIDir))
	if err != nil {
		return fmt.Errorf("the Node API authority (run gen api-ca first): %w", err)
	}
	f, err := client.Issue(authority, *role, *cn, endpoints)
	if err != nil {
		return err
	}
	if err := f.Write(*out); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s for role %s\n", *out, *role)
	return nil
}

// call is a client command: it parses the flags every one takes, dials the Node and runs.
type call struct {
	fs           *flag.FlagSet
	config, node *string
}

func newCall(name string, stderr io.Writer) call {
	fs := newFlags(name, stderr)
	return call{
		fs:     fs,
		config: fs.String("config", "", "the client file written by gen api-client"),
		node:   fs.String("node", "", "the Node's address, an IP address with or without a port (default the client file's first endpoint)"),
	}
}

// parse reads the flags, and the positional arguments that may stand among them, which it returns.
func (c call) parse(args []string, positional int) ([]string, error) {
	var rest []string
	for {
		if err := c.fs.Parse(args); err != nil {
			return nil, err
		}
		if c.fs.NArg() == 0 {
			break
		}
		rest = append(rest, c.fs.Arg(0))
		args = c.fs.Args()[1:]
	}
	if len(rest) != positional {
		return nil, fmt.Errorf("%d argument(s) expected, got %d", positional, len(rest))
	}
	if err := require(c.fs, "config"); err != nil {
		return nil, err
	}
	return rest, nil
}

func (c call) dial() (*client.Client, error) {
	f, err := client.LoadFile(*c.config)
	if err != nil {
		return nil, err
	}
	return client.Dial(f, *c.node)
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

func version(args []string, stdout, stderr io.Writer) error {
	c := newCall("version", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	v, err := cl.Version(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "version  %s\nboot ID  %s\nuptime   %s\n", v.Version, v.BootID, (time.Duration(v.UptimeSeconds) * time.Second).String())
	return nil
}

func reboot(args []string, stdout, stderr io.Writer) error {
	c := newCall("reboot", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	actions, err := cl.Reboot(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, "the Node accepted the reboot:")
	for _, a := range actions {
		_, _ = fmt.Fprintf(stdout, "  %s %s\n", a.Action, a.Target)
	}
	return nil
}

func services(args []string, stdout, stderr io.Writer) error {
	c := newCall("services", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	list, err := cl.Services(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSTATE\tPID\tRESTARTS\tSTARTED")
	for _, s := range list {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n", s.Name, s.State, s.PID, s.Restarts, s.StartedAt)
	}
	return w.Flush()
}

func logs(args []string, stdout, stderr io.Writer) error {
	c := newCall("logs", stderr)
	follow := c.fs.Bool("f", false, "keep the connection open and print each new line")
	rest, err := c.parse(args, 1)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	if *follow {
		cancel()
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt)
	}
	defer cancel()
	return cl.Logs(ctx, rest[0], *follow, stdout)
}

func getConfig(args []string, stdout, stderr io.Writer) error {
	c := newCall("get config", stderr)
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	cfg, err := cl.Config(ctx)
	if err != nil {
		return err
	}
	// The document is the configuration and its status, printed as the one YAML document it is.
	var doc map[string]any
	if err := json.Unmarshal(cfg.Config, &doc); err != nil {
		return fmt.Errorf("GET /v1/config: the answer is not what the Node API sends: %w", err)
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%s", b)
	return err
}

func apply(args []string, stdout, stderr io.Writer) error {
	c := newCall("apply", stderr)
	file := c.fs.String("f", "", "the patch, a YAML or JSON mapping merged into the Node's configuration")
	mode := c.fs.String("mode", client.NoReboot, "no-reboot (live), staged (stored for the next boot) or reboot (stored, then the Node reboots)")
	dryRun := c.fs.Bool("dry-run", false, "list what the apply would do and do none of it")
	if _, err := c.parse(args, 0); err != nil {
		return err
	}
	if err := require(c.fs, "f"); err != nil {
		return err
	}
	patch, err := readPatch(*file)
	if err != nil {
		return err
	}
	cl, err := c.dial()
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout()
	defer cancel()
	cur, err := cl.Config(ctx)
	if err != nil {
		return err
	}
	res, err := cl.Apply(ctx, cur.Revision, patch, *mode, *dryRun)
	if err != nil {
		return err
	}
	if res.DryRun {
		_, _ = fmt.Fprintf(stdout, "dry run at revision %d, mode %s: nothing was done; an apply would:\n", res.Revision, res.Mode)
	} else {
		_, _ = fmt.Fprintf(stdout, "applied revision %d, mode %s:\n", res.Revision, res.Mode)
	}
	for _, a := range res.Actions {
		_, _ = fmt.Fprintf(stdout, "  %s %s\n", a.Action, a.Target)
	}
	return nil
}

// readPatch reads a YAML or JSON mapping. A parser's message is dropped, as it can quote the
// value it refused and a patch may carry a key.
func readPatch(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var patch map[string]any
	if err := yaml.Unmarshal(b, &patch); err != nil || patch == nil {
		return nil, errors.New(path + " is not a YAML or JSON mapping")
	}
	if _, err := json.Marshal(patch); err != nil {
		return nil, errors.New(path + " holds a value JSON cannot carry, such as a key that is not a string")
	}
	return patch, nil
}
