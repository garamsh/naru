package seed_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/seed"
)

func TestSet_MakesTheMissingMapsReplacesAFieldAndKeepsTheRest(t *testing.T) {
	f := filepath.Join(t.TempDir(), "naru.yaml")
	require.NoError(t, os.WriteFile(f, []byte("spec:\n  host:\n    region: eu-1\n  services: [a]\n"), 0o600))

	require.NoError(t, seed.Set(f, map[string]any{"managementPrefix": "10.78.0.0/24"}, "spec", "host", "network"))
	require.NoError(t, seed.Set(f, map[string]any{"listen": "10.0.2.15"}, "spec", "metrics"))
	require.NoError(t, seed.Set(f, "us-1", "spec", "host", "region"))

	var got map[string]map[string]any
	b, err := os.ReadFile(f)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(b, &got))
	host := got["spec"]["host"].(map[string]any)
	assert.Equal(t, "us-1", host["region"])
	assert.Equal(t, map[string]any{"managementPrefix": "10.78.0.0/24"}, host["network"])
	assert.Equal(t, map[string]any{"listen": "10.0.2.15"}, got["spec"]["metrics"])
	assert.Equal(t, []any{"a"}, got["spec"]["services"])
}

func TestSet_RefusesAPathThroughAScalar(t *testing.T) {
	f := filepath.Join(t.TempDir(), "naru.yaml")
	require.NoError(t, os.WriteFile(f, []byte("spec:\n  host: x\n"), 0o600))

	assert.Error(t, seed.Set(f, 1, "spec", "host", "network"))
}

func TestAppend_AddsToAListAndMakesAMissingOne(t *testing.T) {
	f := filepath.Join(t.TempDir(), "naru.yaml")
	require.NoError(t, os.WriteFile(f, []byte("spec:\n  services:\n    - name: a\n"), 0o600))

	require.NoError(t, seed.Append(f, map[string]any{"name": "b"}, "spec", "services"))
	require.NoError(t, seed.Append(f, map[string]any{"path": "/run/x/k.pem"}, "spec", "files"))

	var got map[string]map[string][]map[string]any
	b, err := os.ReadFile(f)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(b, &got))
	assert.Equal(t, []map[string]any{{"name": "a"}, {"name": "b"}}, got["spec"]["services"])
	assert.Equal(t, []map[string]any{{"path": "/run/x/k.pem"}}, got["spec"]["files"])
}

func TestDelete_RemovesTheFieldAndIgnoresAMissingOne(t *testing.T) {
	f := filepath.Join(t.TempDir(), "naru.yaml")
	require.NoError(t, os.WriteFile(f, []byte("spec:\n  orch:\n    name: a\n  signer:\n    x: 1\n"), 0o600))

	require.NoError(t, seed.Delete(f, "spec", "orch"))
	require.NoError(t, seed.Delete(f, "spec", "host", "network"))

	var got map[string]map[string]any
	b, err := os.ReadFile(f)
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(b, &got))
	assert.NotContains(t, got["spec"], "orch")
	assert.Contains(t, got["spec"], "signer")
}

func TestAppend_RefusesAFieldThatIsNotAList(t *testing.T) {
	f := filepath.Join(t.TempDir(), "naru.yaml")
	require.NoError(t, os.WriteFile(f, []byte("spec:\n  services: x\n"), 0o600))

	assert.Error(t, seed.Append(f, 1, "spec", "services"))
}
