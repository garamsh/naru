package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNaruYAML_ASuppliedGaonBinaryReplacesTheImagesAndOnlyThatOne(t *testing.T) {
	c := gaonCfg(t, 1, 6443)
	c.Gaon.Overrides = []string{"gaon-apiserver", "gaon-node-agent"}
	n := parse(t, c)

	assert.Equal(t, "/var/lib/naru/inputs/gaon/gaon-apiserver", serviceNamed(t, n, "gaon-apiserver").Binary)
	assert.Equal(t, "/var/lib/naru/inputs/gaon/gaon-node-agent", serviceNamed(t, n, "gaon-node-agent").Binary)
	assert.Equal(t, "/bin/gaon-scheduler", serviceNamed(t, n, "gaon-scheduler").Binary, "a binary not supplied is the image's")
	assert.Equal(t, "/bin/gaon-controllers", serviceNamed(t, n, "gaon-controllers").Binary)
}

func TestNaruYAML_ADisabledServiceIsLeftOutOfEveryNodeAndABinaryLeftOutDoesNotDisableIt(t *testing.T) {
	for _, index := range []int{1, 2} {
		c := gaonCfg(t, index, 0)
		c.Disabled = []string{"gaon-controllers"}
		assert.NotContains(t, names(parse(t, c)), "gaon-controllers", "node-%d", index)
		assert.Contains(t, names(parse(t, c)), "gaon-scheduler", "the others stay")
	}
	c := gaonCfg(t, 1, 6443)
	c.Gaon.Overrides = []string{"gaon-apiserver"}
	assert.Contains(t, names(parse(t, c)), "gaon-controllers", "supplying no gaon-controllers binary leaves the service on")
}

func TestNaruYAML_ADisabledServiceIsLeftOutOfTheRestartListsOfSpecPKI(t *testing.T) {
	c := bootPKICfg(t, 1)
	c.Disabled = []string{"gaon-controllers"}
	n := parse(t, c)

	require.NotNil(t, n.Spec.PKI)
	var issued bool
	for _, cert := range n.Spec.PKI.Certificates {
		assert.NotContains(t, cert.Restart, "gaon-controllers", cert.Name)
		if cert.Name == "controllers" {
			issued = true
		}
	}
	assert.True(t, issued, "the pair is still issued; only the restart is dropped")
}
