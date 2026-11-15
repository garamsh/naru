package layout_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/layout"
)

// What layout writes, lib/init reads: the writer and the reader are two modules with two YAML
// libraries (ADR 0070), and this is the one place a field one renames and the other does not shows.
func TestUserData_IsWhatInitsConfigParses(t *testing.T) {
	hostCfg, _ := hostNode(t)
	p := profile(t)
	orchHost, err := p.Node(joined(t))
	require.NoError(t, err)

	for name, n := range map[string]layout.Node{
		"a worker":                 node(t, func(*layout.Node) {}),
		"a control plane":          node(t, func(n *layout.Node) { n.Mode, n.Tokens = layout.ControlPlaneToken, []string{tokenA} }),
		"a control plane of files": node(t, func(n *layout.Node) { n.Mode, n.Token = layout.ControlPlaneFiles, "" }),
		"a host":                   hostCfg,
		"the host orch builds":     orchHost,
	} {
		t.Run(name, func(t *testing.T) {
			data, err := n.UserData()
			require.NoError(t, err)
			cfg, err := config.Parse(data)
			require.NoError(t, err)
			assert.Contains(t, cfg.Metadata.Name, n.Name)
		})
	}
}
