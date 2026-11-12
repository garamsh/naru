package layout_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/layout"
)

// emitted are the services of a Node's user-data after mvmd, with whether each is marked control-plane.
func emitted(t *testing.T, n layout.Node) (names []string, controlPlane map[string]bool) {
	t.Helper()
	data, err := n.UserData()
	require.NoError(t, err)
	var c struct {
		Spec struct {
			Services []struct {
				Name         string `yaml:"name"`
				ControlPlane bool   `yaml:"controlPlane"`
			} `yaml:"services"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &c))
	controlPlane = map[string]bool{}
	for _, s := range c.Spec.Services {
		if s.Name == "mvmd" {
			continue
		}
		names = append(names, s.Name)
		controlPlane[s.Name] = s.ControlPlane
	}
	return names, controlPlane
}

func TestTopology_IsWhatUserDataEmitsInEveryMode(t *testing.T) {
	modes := map[string]layout.Mode{
		"worker, token": layout.WorkerToken, "worker, files": layout.WorkerFiles,
		"control plane, token": layout.ControlPlaneToken, "control plane, files": layout.ControlPlaneFiles,
	}
	for name, m := range modes {
		t.Run(name, func(t *testing.T) {
			n := node(t, func(n *layout.Node) {
				n.Mode = m
				if m == layout.ControlPlaneToken {
					n.Tokens = []string{tokenA}
				}
				if m.PairsAsFiles() {
					n.Token = ""
				}
			})
			names, controlPlane := emitted(t, n)

			assert.Equal(t, layout.Gaon.Names(), names, "the user-data runs the topology's services in its order")
			for _, s := range layout.Gaon.Services {
				assert.Equal(t, s.ControlPlane, controlPlane[s.Name], "%s: controlPlane mark", s.Name)
			}
		})
	}
}

func TestTopology_RunningLeavesOutTheControlPlaneServicesOnAWorker(t *testing.T) {
	var worker []string
	for _, s := range layout.Gaon.Running(false) {
		worker = append(worker, s.Name)
	}

	assert.Equal(t, []string{"registry", "gaon-node-agent"}, worker)
	assert.Equal(t, layout.Gaon.Names(), func() []string {
		var all []string
		for _, s := range layout.Gaon.Running(true) {
			all = append(all, s.Name)
		}
		return all
	}())
}

func TestTopology_TheSignerTheAPIServerAndTheAgentAreServicesOfTheTopology(t *testing.T) {
	for _, name := range []string{layout.Gaon.Signer, layout.Gaon.APIServer, layout.Gaon.Agent} {
		assert.True(t, layout.Gaon.Has(name), name)
	}
	assert.True(t, layout.Gaon.OnlyControlPlane(layout.Gaon.Signer))
	assert.True(t, layout.Gaon.OnlyControlPlane(layout.Gaon.APIServer))
	assert.False(t, layout.Gaon.OnlyControlPlane(layout.Gaon.Agent))
}

func TestTopology_JSONIsWhatTheHarnessDecodes(t *testing.T) {
	b, err := layout.Gaon.JSON()
	require.NoError(t, err)

	var got layout.Topology
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, layout.Gaon, got)
	assert.Contains(t, string(b), `"controlPlane": true`)
	assert.Contains(t, string(b), `"signer": "gaon-controllers"`)
}

func TestMode_OfTheTwoFlagsHasOneValueEach(t *testing.T) {
	for _, c := range []struct {
		controlPlane, files bool
		want                layout.Mode
	}{
		{false, false, layout.WorkerToken}, {false, true, layout.WorkerFiles},
		{true, false, layout.ControlPlaneToken}, {true, true, layout.ControlPlaneFiles},
	} {
		m := layout.ModeOf(c.controlPlane, c.files)
		assert.Equal(t, c.want, m)
		assert.Equal(t, c.controlPlane, m.ControlPlane())
		assert.Equal(t, c.files, m.PairsAsFiles())
		assert.Equal(t, c.want == layout.ControlPlaneToken, m.Signs())
	}
}
