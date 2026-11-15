package command

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// removedIn is the release that stops accepting the flag spellings ADR 0067 deprecates.
const removedIn = "v0.1.0-dev.13"

// runner runs a command once cobra has parsed its flags; args are the positional arguments.
type runner func(args []string, stdout, stderr io.Writer) error

// binder declares a command's flags on fs and returns what runs when they are parsed. The flag
// values it declares are the closure the runner reads.
type binder func(fs *pflag.FlagSet) runner

// aliasKey marks a flag as a deprecated spelling of the flag the annotation names.
const aliasKey = "naructl-alias"

// aliasString registers old as a hidden spelling of the string flag new, which p holds. Using it
// works as using the flag and prints a deprecation line.
func aliasString(fs *pflag.FlagSet, p *string, old, new string) {
	fs.StringVar(p, old, *p, "deprecated spelling of --"+new)
	_ = fs.MarkHidden(old)
	_ = fs.SetAnnotation(old, aliasKey, []string{new})
}

// setFlags is the names of the flags the command line gave, a deprecated spelling counted as the
// flag it stands for.
func setFlags(fs *pflag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *pflag.Flag) {
		if a, ok := f.Annotations[aliasKey]; ok {
			set[a[0]] = true
			return
		}
		set[f.Name] = true
	})
	return set
}

// warnAliases prints one line for each deprecated spelling the command line used.
func warnAliases(fs *pflag.FlagSet, stderr io.Writer) {
	fs.Visit(func(f *pflag.Flag) {
		if a, ok := f.Annotations[aliasKey]; ok {
			_, _ = fmt.Fprintf(stderr, "naructl: --%s is deprecated, use --%s (it stops working in %s)\n", f.Name, a[0], removedIn)
		}
	})
}

// leaf is a command that does something.
func leaf(use, short string, b binder) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, Args: cobra.ArbitraryArgs}
	run := b(c.Flags())
	c.RunE = func(c *cobra.Command, args []string) error {
		warnAliases(c.Flags(), c.ErrOrStderr())
		return run(args, c.OutOrStdout(), c.ErrOrStderr())
	}
	return c
}

// described sets the long help and the examples of a command.
func described(c *cobra.Command, long, example string) *cobra.Command {
	c.Long, c.Example = long, example
	return c
}

// parent is a command that only holds subcommands: without one, or with one it does not have, it
// refuses and says which there are.
func parent(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, Args: cobra.ArbitraryArgs}
	c.AddCommand(subs...)
	c.RunE = func(c *cobra.Command, args []string) error {
		var names []string
		for _, s := range c.Commands() {
			if s.IsAvailableCommand() {
				names = append(names, s.Name())
			}
		}
		list := strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
		if len(args) == 0 {
			return fmt.Errorf("%s needs %s", c.CommandPath(), list)
		}
		return fmt.Errorf("%s: %q is not %s", c.CommandPath(), args[0], list)
	}
	return c
}

// runnableParent is a parent that does something itself when given no subcommand.
func runnableParent(use, short string, b binder, subs ...*cobra.Command) *cobra.Command {
	c := leaf(use, short, b)
	c.AddCommand(subs...)
	return c
}

// positional refuses the arguments of a command given a number other than n.
func positional(args []string, n int) error {
	if len(args) != n {
		return fmt.Errorf("%d argument(s) expected, got %d", n, len(args))
	}
	return nil
}

// serverNameFlag declares --server-name, which every command that dials a Node API or a host API
// takes: the name the server's certificate is verified against in place of its address.
func serverNameFlag(fs *pflag.FlagSet) *string {
	return fs.String("server-name", "", "the name to verify the server's certificate against instead of its address: the host ID h-<id> of a host that joined from the install seed")
}
