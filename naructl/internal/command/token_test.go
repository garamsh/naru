package command_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stored is the token the store holds for the Node.
func stored(t *testing.T, ca, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ca, "tokens", name))
	require.NoError(t, err)
	return string(b)
}

// patchedFile is the one file of the spec.files the last POST carried.
func patchedFile(t *testing.T, posted map[string]any) map[string]any {
	t.Helper()
	files := posted["spec"].(map[string]any)["files"].([]any)
	require.Len(t, files, 1)
	return files[0].(map[string]any)
}

func TestGenNode_TheTokensFileRestartsTheAPIServerSoAPatchToItTakesEffect(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-1", "--control-plane"))

	for _, f := range u.Spec.Files {
		switch f.Path {
		case "/run/gaon/bootstrap-tokens":
			assert.Equal(t, []string{"gaon-apiserver"}, f.Restart)
		default:
			assert.Empty(t, f.Restart, f.Path)
		}
	}
}

func TestTokenCreate_AddsTheNodesTokenToTheStoreAndPatchesTheWholeListLive(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "admin")
	mustRun(t, "gen", "token", "--ca-dir", dep.ca, "--name", "node-1")
	_, err := os.Stat(filepath.Join(dep.ca, "tokens", "node-3"))
	require.ErrorIs(t, err, os.ErrNotExist, "setup: node-3 has no token yet")

	out, err := run(t, "token", "create", "--ca-dir", dep.ca, "--name", "node-3", "--config", cfg)

	require.NoError(t, err, out)
	f := patchedFile(t, node.posted)
	assert.Equal(t, "/run/gaon/bootstrap-tokens", f["path"])
	assert.Equal(t, stored(t, dep.ca, "node-1")+stored(t, dep.ca, "node-2")+stored(t, dep.ca, "node-3"), f["content"], "every token of the store, in the order of the names")
	assert.Equal(t, "0600", f["mode"])
	assert.Equal(t, []any{"gaon-apiserver"}, f["restart"])
	assert.Equal(t, "no-reboot", node.postMode)
}

func TestTokenRevoke_DeletesTheTokenAndPatchesTheListWithoutIt(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "admin")
	mustRun(t, "gen", "token", "--ca-dir", dep.ca, "--name", "node-1")
	mustRun(t, "gen", "token", "--ca-dir", dep.ca, "--name", "node-3")
	three := stored(t, dep.ca, "node-3")

	out, err := run(t, "token", "revoke", "--ca-dir", dep.ca, "--name", "node-3", "--config", cfg)

	require.NoError(t, err, out)
	_, err = os.Stat(filepath.Join(dep.ca, "tokens", "node-3"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	content := patchedFile(t, node.posted)["content"]
	assert.NotContains(t, content, three)
	assert.Equal(t, stored(t, dep.ca, "node-1")+stored(t, dep.ca, "node-2"), content, "the others stay")
}

func TestTokenRevoke_ATokenThatWasNeverMadeIsRefusedAndNothingIsPatched(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "admin")

	out, err := run(t, "token", "revoke", "--ca-dir", dep.ca, "--name", "node-9", "--config", cfg)

	require.Error(t, err)
	assert.Contains(t, out+err.Error(), "holds no token")
	assert.Empty(t, node.requests)
}
