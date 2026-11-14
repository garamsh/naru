package command_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const networkYAML = `name: lab
prefix: 10.9.0.0/24
apiEndpoint: 10.9.0.2
`

func TestNetworkApply_SendsTheNameAndSpecTheFileDeclares(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "network", "apply", "-f", writeGroup(t, networkYAML))...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "network lab applied: 10.9.0.0/24")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.networks, 1)
	assert.Equal(t, "lab", f.networks[0].name)
	assert.Equal(t, "10.9.0.0/24", f.networks[0].spec.Prefix)
	assert.Equal(t, "10.9.0.2", f.networks[0].spec.APIEndpoint)
}

func TestNetworkApply_RefusesAFileThatIsNotANetwork(t *testing.T) {
	for name, body := range map[string]string{
		"a field networks do not have": networkYAML + "gateway: 10.9.0.1\n",
		"no name":                      "prefix: 10.9.0.0/24\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOrch(t)
			op := newOperator(t, f, "operator")
			_, err := run(t, op.args(f, "network", "apply", "-f", writeGroup(t, body))...)
			require.Error(t, err)
			f.mu.Lock()
			defer f.mu.Unlock()
			assert.Empty(t, f.networks)
		})
	}

	// The control: the file the cases vary is applied.
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	_, err := run(t, op.args(f, "network", "apply", "-f", writeGroup(t, networkYAML))...)
	require.NoError(t, err)
}

func TestNetworkGetListDelete(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "network", "get", "lab")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "network lab: prefix 10.9.0.0/24, API endpoint 10.9.0.2")

	out, err = run(t, op.args(f, "network", "list")...)
	require.NoError(t, err, out)
	assert.Regexp(t, `lab\s+10\.9\.0\.0/24\s+10\.9\.0\.2`, out)
	assert.Regexp(t, `edge\s+10\.8\.0\.0/24`, out)

	out, err = run(t, op.args(f, "network", "delete", "lab")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "network lab deleted")
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []string{"lab"}, f.deletedNetworks)

	_, err = run(t, op.args(f, "network", "get")...)
	require.Error(t, err)
	_, err = run(t, op.args(f, "network", "frobnicate")...)
	require.Error(t, err)
}
