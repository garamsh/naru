package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/naructl/internal/client"
)

// callTimeout bounds every call but a followed log, which runs until it is interrupted.
const callTimeout = 30 * time.Second

func genAPIClient(fs *pflag.FlagSet) runner {
	var endpoints list
	caDir := fs.String("ca-dir", "", "the cluster's directory that holds api/ca.pem and api/ca.key")
	role := fs.String("role", "", "admin (reads and applies) or reader (reads)")
	cn := fs.String("cn", "", "the certificate's CN, the caller's name in a Node's request log (default naru-<role>)")
	out := fs.String("out", "", "the client file to write (0600: it carries a key; a file already there is replaced)")
	fs.Var(&endpoints, "endpoint", "a Node's address, an IP address with or without a port, repeatable")
	return func(args []string, stdout, stderr io.Writer) error {
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
}

// call is a client command: the flags every one takes, then dial and run.
type call struct {
	fs                           *pflag.FlagSet
	config, endpoint, serverName *string
}

func newCall(fs *pflag.FlagSet) call {
	c := call{
		fs:         fs,
		config:     fs.String("config", "", "the client file written by gen api-client"),
		endpoint:   fs.String("endpoint", "", "the Node's address, an IP address with or without a port (default the client file's first endpoint)"),
		serverName: serverNameFlag(fs),
	}
	return c
}

// check refuses a command given a number of arguments other than positional, or no --config.
func (c call) check(args []string, n int) error {
	if err := positional(args, n); err != nil {
		return err
	}
	return require(c.fs, "config")
}

func (c call) dial() (*client.Client, error) {
	f, err := client.LoadFile(*c.config)
	if err != nil {
		return nil, err
	}
	return client.Dial(f, *c.endpoint, *c.serverName)
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), callTimeout)
}

func version(fs *pflag.FlagSet) runner {
	c := newCall(fs)
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
		v, err := cl.Version(ctx)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "version  %s\nboot ID  %s\nuptime   %s\n", v.Version, v.BootID, (time.Duration(v.UptimeSeconds) * time.Second).String())
		return nil
	}
}

func reboot(fs *pflag.FlagSet) runner {
	c := newCall(fs)
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
}

func services(fs *pflag.FlagSet) runner {
	c := newCall(fs)
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
		list, err := cl.Services(ctx)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "NAME\tSTATE\tPID\tRESTARTS\tSTARTED\tEXTENSION")
		for _, s := range list {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n", s.Name, s.State, s.PID, s.Restarts, s.StartedAt, s.Extension)
		}
		return w.Flush()
	}
}

func logs(fs *pflag.FlagSet) runner {
	c := newCall(fs)
	follow := c.fs.BoolP("follow", "f", false, "keep the connection open and print each new line")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 1); err != nil {
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
		return cl.Logs(ctx, args[0], *follow, stdout)
	}
}

func getConfig(fs *pflag.FlagSet) runner {
	c := newCall(fs)
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
}

func apply(fs *pflag.FlagSet) runner {
	c := newCall(fs)
	file := c.fs.StringP("file", "f", "", "the patch, a YAML or JSON mapping merged into the Node's configuration")
	mode := c.fs.String("mode", client.NoReboot, "no-reboot (live), staged (stored for the next boot) or reboot (stored, then the Node reboots)")
	dryRun := c.fs.Bool("dry-run", false, "list what the apply would do and do none of it")
	return func(args []string, stdout, stderr io.Writer) error {
		if err := c.check(args, 0); err != nil {
			return err
		}
		if err := require(c.fs, "file"); err != nil {
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
