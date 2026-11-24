package command

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/naructl/internal/client"
)

// tokenCommand adds or revokes a Node's bootstrap token: it changes the token store under --ca-dir,
// the operator's record of every token, then patches the control-plane Node's list of them live
// (ADR 0081). The patch carries the whole list from the store, because the Node API redacts the
// list's content on a read and a merge cannot be built from it.
func tokenCommand(create bool) func(*pflag.FlagSet) runner {
	return func(fs *pflag.FlagSet) runner {
		c := newCall(fs)
		caDir := fs.String("ca-dir", "", "directory of the cluster's authority, which holds tokens/")
		name := fs.String("name", "", "the Node's name, such as node-3")
		return func(args []string, stdout, stderr io.Writer) error {
			if err := c.check(args, 0); err != nil {
				return err
			}
			if err := require(fs, "ca-dir", "name"); err != nil {
				return err
			}
			cl, err := c.dial()
			if err != nil {
				return err
			}
			if create {
				_, err = pki.EnsureToken(*caDir, *name)
			} else if err = pki.RemoveToken(*caDir, *name); errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%s holds no token", filepath.Join(*caDir, pki.TokensDir, *name))
			}
			if err != nil {
				return err
			}
			tokens, err := pki.ReadTokens(*caDir)
			if err != nil {
				return err
			}
			path, content, restart := layout.BootstrapTokensFile, layout.TokensContent(tokens), layout.Gaon.APIServer
			// The mode and restart are the file's own, repeated so a Node that lacks the file gets it as gen node writes it.
			patch := map[string]any{"spec": map[string]any{"files": []any{map[string]any{
				"path": path, "mode": "0600", "content": content, "restart": []any{restart},
			}}}}
			ctx, cancel := withTimeout()
			defer cancel()
			cur, err := cl.Config(ctx)
			if err != nil {
				return err
			}
			res, err := cl.Apply(ctx, cur.Revision, patch, client.NoReboot, false)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(stdout, "applied revision %d, mode %s:\n", res.Revision, res.Mode)
			for _, a := range res.Actions {
				_, _ = fmt.Fprintf(stdout, "  %s %s\n", a.Action, a.Target)
			}
			return nil
		}
	}
}
