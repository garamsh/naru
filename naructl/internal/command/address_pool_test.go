package command_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const addressPoolYAML = `name: lan
cidrs: [192.168.0.80/29]
networks: [lab]
`

func TestAddressPoolApply_SendsTheNameAndSpecTheFileDeclares(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "address-pool", "apply", "-f", writeGroup(t, addressPoolYAML))...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "address pool lan applied: 192.168.0.80/29 for lab")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.pools, 1)
	assert.Equal(t, "lan", f.pools[0].name)
	assert.Equal(t, []string{"192.168.0.80/29"}, f.pools[0].spec.CIDRs)
	assert.Equal(t, []string{"lab"}, f.pools[0].spec.Networks)
}

func TestAddressPoolApply_RefusesAFileThatIsNotAPool(t *testing.T) {
	for name, body := range map[string]string{
		"a field pools do not have": addressPoolYAML + "gateway: 192.168.0.1\n",
		"no name":                   "cidrs: [192.168.0.80/29]\nnetworks: [lab]\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOrch(t)
			op := newOperator(t, f, "operator")
			_, err := run(t, op.args(f, "address-pool", "apply", "-f", writeGroup(t, body))...)
			require.Error(t, err)
			f.mu.Lock()
			defer f.mu.Unlock()
			assert.Empty(t, f.pools)
		})
	}

	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	_, err := run(t, op.args(f, "address-pool", "apply", "-f", writeGroup(t, addressPoolYAML))...)
	require.NoError(t, err, "control: the file the cases vary is applied")
}

func TestAddressPoolListAndDeleteAndExposureListAndRelease(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "address-pool", "list")...)
	require.NoError(t, err, out)
	assert.Regexp(t, `lan\s+192\.168\.0\.80/29\s+lab`, out)
	out, err = run(t, op.args(f, "address-pool", "delete", "lan")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "address pool lan deleted")
	out, err = run(t, op.args(f, "exposure", "list")...)
	require.NoError(t, err, out)
	assert.Regexp(t, `lab\s+d\.web\.0a1b2c3d\s+192\.168\.0\.80\s+h1\s+announced`, out)
	out, err = run(t, op.args(f, "exposure", "delete", "lab", "d.web.0a1b2c3d")...)
	require.NoError(t, err, out)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []string{"lan"}, f.deletedPools)
	assert.Equal(t, []string{"lab/d.web.0a1b2c3d"}, f.releasedExposures)

	_, err = run(t, op.args(f, "exposure", "delete", "lab")...)
	require.Error(t, err, "an exposure is named by its network and id")
}
