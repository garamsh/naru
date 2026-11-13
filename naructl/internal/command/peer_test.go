package command_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

type routedUserData struct {
	Spec struct {
		Network struct {
			Routes []struct{ To, Via string }
		}
	}
}

func routes(t *testing.T, path string) []struct{ To, Via string } {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var u routedUserData
	require.NoError(t, yaml.Unmarshal(b, &u))
	return u.Spec.Network.Routes
}

// mvmdNetwork returns the network section of the mvmd configuration a user-data file carries.
func mvmdNetwork(t *testing.T, path string) map[string]any {
	t.Helper()
	u := parse(t, path)
	content, ok := u.file("/run/mvmd/config.json")
	require.True(t, ok)
	var cfg struct{ Network map[string]any }
	require.NoError(t, json.Unmarshal([]byte(content), &cfg))
	return cfg.Network
}

func TestGenNode_PeersAreRoutedToViaTheirAddressAndLeftUnmasqueraded(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1", "--peer", "192.168.0.20=10.200.2.0/24", "--peer", "192.168.0.30=10.200.3.0/24")

	assert.Equal(t, []struct{ To, Via string }{{"10.200.2.0/24", "192.168.0.20"}, {"10.200.3.0/24", "192.168.0.30"}}, routes(t, path))
	assert.Equal(t, []any{"10.200.2.0/24", "10.200.3.0/24"}, mvmdNetwork(t, path)["no_masquerade"])
	assert.Equal(t, "10.200.1.0/24", mvmdNetwork(t, path)["ipv4_prefix"], "the Node's own prefix is the one mvmd serves")
	validate(t, path) // naru-init accepts the routes
}

func TestGenNode_WithoutPeersThereAreNoRoutesAndNoNoMasquerade(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1")

	assert.Empty(t, routes(t, path))
	assert.NotContains(t, mvmdNetwork(t, path), "no_masquerade")
}

func TestGenNode_RefusesAPeerItCannotRouteTo(t *testing.T) {
	dep := newDeployment(t)
	tests := []struct {
		name   string
		peers  []string
		reason string
	}{
		{"not <address>=<prefix>", []string{"192.168.0.20"}, "is not <address>=<machine-prefix>"},
		{"an address that is no address", []string{"next-door=10.200.2.0/24"}, "--peer"},
		{"an address off the Node's network", []string{"10.9.9.9=10.200.2.0/24"}, "not on the Node's network"},
		{"the Node's own address", []string{"192.168.0.10=10.200.2.0/24"}, "the Node's own"},
		{"a prefix with host bits", []string{"192.168.0.20=10.200.2.5/24"}, "host bits"},
		{"a prefix that is the Node's own", []string{"192.168.0.20=10.200.1.0/24"}, "overlaps the Node's own"},
		{"a prefix inside the Node's own", []string{"192.168.0.20=10.200.1.128/25"}, "overlaps the Node's own"},
		{"a prefix that overlaps the Node's network", []string{"192.168.0.20=192.168.0.0/16"}, "overlaps the Node's network"},
		{"the same address twice", []string{"192.168.0.20=10.200.2.0/24", "192.168.0.20=10.200.3.0/24"}, "listed twice"},
		{"two overlapping prefixes", []string{"192.168.0.20=10.200.2.0/24", "192.168.0.30=10.200.2.0/25"}, "overlapping Machine prefixes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{
				"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
				"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
			}
			for _, p := range tt.peers {
				args = append(args, "--peer", p)
			}

			out, err := run(t, args...)

			require.Error(t, err)
			assert.Contains(t, out, tt.reason)
		})
	}
}
