package command_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

func TestGenNode_TimeServersAreWrittenInOrderAndInitAcceptsThem(t *testing.T) {
	dep := newDeployment(t)

	path := dep.node(t, "node-2", "--time-server", "ntp.example.net", "--time-server", "192.0.2.7:1123")

	assert.Contains(t, validate(t, path), "valid; node=naru-node-2")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var got struct {
		Spec struct {
			Time *struct{ Servers []string } `yaml:"time"`
		}
	}
	require.NoError(t, yaml.Unmarshal(b, &got))
	require.NotNil(t, got.Spec.Time)
	assert.Equal(t, []string{"ntp.example.net", "192.0.2.7:1123"}, got.Spec.Time.Servers)
}

func TestGenNode_WithoutTimeServerTheUserDataHasNoSpecTime(t *testing.T) {
	// The control is the test above: with a flag, spec.time is written.
	dep := newDeployment(t)

	path := dep.node(t, "node-2")

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "time:")
}

func TestGenNode_InitRefusesATimeServerThatIsNoHost(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-2", "--time-server", "not a host")

	out, err := exec.Command(naruInit(t), "-validate", path).CombinedOutput() //nolint:gosec // the binary built for these tests

	require.Error(t, err)
	assert.Contains(t, string(out), "spec.time.servers[0]")
}
