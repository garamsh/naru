package command_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const groupYAML = `name: web
count: 3
region: kr-seoul
template:
  form: image
  cpus: 1
  memoryMiB: 128
  version: r@sha256:abc
rollout:
  maxUnavailable: 1
`

func writeGroup(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "web.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestGroupApply_SendsTheSpecTheFileDeclares(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "group", "apply", "-f", writeGroup(t, groupYAML))...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "group web applied")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.groups, 1)
	assert.Equal(t, "web", f.groups[0].Name)
	assert.Equal(t, 3, f.groups[0].Count)
	assert.Equal(t, "r@sha256:abc", f.groups[0].Template.Version)
	assert.Equal(t, 1, f.groups[0].Rollout.MaxUnavailable)
}

func TestGroupApply_RefusesAFieldTheSpecDoesNotHave(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	_, err := run(t, op.args(f, "group", "apply", "-f", writeGroup(t, groupYAML+"replicas: 4\n"))...)
	require.Error(t, err)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, f.groups)
}

func TestGroupGetListDelete(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "group", "get", "web")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "group web: 1 wanted")
	assert.Contains(t, out, "web-1")
	assert.Contains(t, out, "host-a")

	out, err = run(t, op.args(f, "group", "list")...)
	require.NoError(t, err, out)
	assert.Regexp(t, `web\s+kr-seoul\s+2\s+1\s+2`, out)

	out, err = run(t, op.args(f, "group", "delete", "web")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "group web deleted")
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []string{"web"}, f.deleted)

	_, err = run(t, op.args(f, "group", "get")...)
	require.Error(t, err)
}
