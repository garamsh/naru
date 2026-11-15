package command

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	req "github.com/stretchr/testify/require"
)

// walk calls f on every command under c, c first.
func walk(c *cobra.Command, f func(*cobra.Command)) {
	f(c)
	for _, s := range c.Commands() {
		walk(s, f)
	}
}

func TestTree_EveryCommandAnswersHelpWithExitZero(t *testing.T) {
	var paths [][]string
	walk(newRoot(), func(c *cobra.Command) {
		paths = append(paths, strings.Fields(strings.TrimPrefix(c.CommandPath(), "naructl")))
	})
	req.Greater(t, len(paths), 80, "the walk found the whole tree")
	for _, p := range paths {
		var out, errOut bytes.Buffer
		err := Run(append(append([]string{}, p...), "-h"), &out, &errOut)
		name := "naructl " + strings.Join(p, " ")
		req.NoError(t, err, name)
		text := out.String() + errOut.String()
		assert.NotContains(t, text, "help requested", name)
		assert.Contains(t, text, "Usage:", name)
	}
}

func TestTree_UsageNamesTheCommandsTheOldUsageForgot(t *testing.T) {
	var buf bytes.Buffer
	req.NoError(t, Run([]string{"network", "-h"}, &buf, &buf))
	for _, sub := range []string{"apply", "get", "list", "delete"} {
		assert.Contains(t, buf.String(), sub)
	}
	buf.Reset()
	req.NoError(t, Run([]string{"signer", "-h"}, &buf, &buf))
	assert.Contains(t, buf.String(), "reissue-root")
}

func TestTree_CompletionIsGeneratedForBashAndZsh(t *testing.T) {
	for shell, marker := range map[string]string{"bash": "bash completion", "zsh": "#compdef"} {
		var out, errOut bytes.Buffer
		req.NoError(t, Run([]string{"completion", shell}, &out, &errOut), shell)
		assert.Contains(t, out.String(), marker, shell)
		assert.Contains(t, out.String(), "naructl", shell)
	}
}

func TestTree_AnErrorIsPrintedOnceByTheCaller(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run([]string{"version", "--no-such-flag"}, &out, &errOut)
	req.Error(t, err)
	assert.Contains(t, err.Error(), "no-such-flag")
	assert.Empty(t, out.String()+errOut.String(), "the command leaves printing the error to main")
}

func TestTree_ArgumentsAfterDoubleDashAreNotParsedAsFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	err := Run([]string{"version", "--", "--follow"}, &out, &errOut)
	req.Error(t, err)
	assert.Contains(t, err.Error(), "got 1", "--follow after -- is an argument, not a flag")
}

func TestTree_UnknownCommandIsRefused(t *testing.T) {
	_, err := runString("frobnicate")
	req.ErrorContains(t, err, `unknown command "frobnicate"`)
	_, err = runString("host", "frobnicate")
	req.Error(t, err)
}

func runString(args ...string) (string, error) {
	var out, errOut bytes.Buffer
	err := Run(args, &out, &errOut)
	return out.String() + errOut.String(), err
}

// The patterns find a naructl command in a document: in a code span, or alone on a line.
var (
	spanOrLine = regexp.MustCompile("`(naructl [^`]+)`|^\\s*(naructl [^`]+)$")
	wordsOnly  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	flagToken  = regexp.MustCompile(`(?:^|[\s\[|(])(--[a-z][a-z0-9-]*)`)
)

// TestDocs_EveryNaructlInvocationNamesACommandAndFlagsItHas reads each document under docs/ and
// checks that the naructl commands it shows exist and take the flags it gives them, none of them a
// deprecated spelling.
func TestDocs_EveryNaructlInvocationNamesACommandAndFlagsItHas(t *testing.T) {
	root := filepath.Join("..", "..", "..", "docs")
	if _, err := os.Stat(root); err != nil {
		t.Skip("the documents are not beside this module")
	}
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".md" || strings.Contains(path, string(filepath.Separator)+"convention"+string(filepath.Separator)) {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := strings.ReplaceAll(string(b), "\\\n", " ")
		for _, line := range strings.Split(text, "\n") {
			for _, m := range spanOrLine.FindAllStringSubmatch(line, -1) {
				inv := m[1] + m[2]
				checked += checkInvocation(t, path, inv)
			}
		}
		return nil
	})
	req.NoError(t, err)
	assert.Greater(t, checked, 30, "the documents' invocations were found")
}

// checkInvocation checks one invocation and returns 1 when it had flags to check.
func checkInvocation(t *testing.T, path, inv string) int {
	t.Helper()
	fields := strings.Fields(strings.TrimPrefix(inv, "naructl "))
	var words []string
	for _, f := range fields {
		if !wordsOnly.MatchString(f) {
			break
		}
		words = append(words, f)
	}
	rest := strings.Join(fields[len(words):], " ")
	flags := flagToken.FindAllStringSubmatch(" "+rest, -1)
	if len(words) == 0 || len(flags) == 0 {
		return 0
	}
	cmd, _, err := newRoot().Find(words)
	req.NoError(t, err, "%s: %s", path, inv)
	for len(words) > 0 && cmd.Name() != words[len(words)-1] {
		words = words[:len(words)-1] // a word that is an argument, not a subcommand
	}
	for _, m := range flags {
		name := strings.TrimPrefix(m[1], "--")
		f := cmd.Flags().Lookup(name)
		if f == nil {
			f = cmd.InheritedFlags().Lookup(name)
		}
		if assert.NotNil(t, f, "%s: %q has no --%s (%s)", path, cmd.CommandPath(), name, inv) {
			assert.False(t, f.Hidden, "%s: --%s of %q is a deprecated spelling (%s)", path, name, cmd.CommandPath(), inv)
		}
	}
	return 1
}
