package command_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
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
	assert.Regexp(t, `lab\s+d\.web\.0a1b2c3d\s+operator\s+192\.168\.0\.80\s+h1\s+announced`, out)
	out, err = run(t, op.args(f, "exposure", "delete", "lab", "d.web.0a1b2c3d")...)
	require.NoError(t, err, out)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []string{"lan"}, f.deletedPools)
	assert.Equal(t, []string{"lab/d.web.0a1b2c3d"}, f.releasedExposures)

	_, err = run(t, op.args(f, "exposure", "delete", "lab")...)
	require.Error(t, err, "an exposure is named by its network and id")
}

const exposureYAML = `id: api
network: lab
pool: lan
requestedAddress: 192.168.0.112
ports: [{protocol: TCP, port: 6443, backendPort: 6443}]
backends: [10.9.0.5]
`

func TestExposureApply_SendsTheSpecTheFileDeclaresToTheNetworksRoute(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "exposure", "apply", "-f", writeGroup(t, exposureYAML))...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "exposure api of network lab applied: address 192.168.0.112, pending")
	out, err = run(t, op.args(f, "exposure", "get", "lab", "api")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"owner": "operator"`)

	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.declared["lab/api"]
	assert.Equal(t, "192.168.0.112", got.RequestedAddress)
	assert.Equal(t, []orchapi.ExposurePort{{Protocol: "TCP", Port: 6443, BackendPort: 6443}}, got.Ports)
	assert.Equal(t, []string{"10.9.0.5"}, got.Backends)
}

func TestExposureApply_RefusesAFileThatIsNotAnExposure(t *testing.T) {
	for name, body := range map[string]string{
		"a field exposures do not have": exposureYAML + "nodePort: 30443\n",
		"no id":                         strings.Replace(exposureYAML, "id: api\n", "", 1),
		"no network":                    strings.Replace(exposureYAML, "network: lab\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOrch(t)
			op := newOperator(t, f, "operator")
			_, err := run(t, op.args(f, "exposure", "apply", "-f", writeGroup(t, body))...)
			require.Error(t, err)
			f.mu.Lock()
			defer f.mu.Unlock()
			assert.Empty(t, f.declared)
		})
	}
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	_, err := run(t, op.args(f, "exposure", "apply", "-f", writeGroup(t, exposureYAML))...)
	require.NoError(t, err, "control: the file the cases vary is applied")
}

const proxyExposureYAML = `id: web
pool: edge
requestedAddress: 192.168.1.81
ports: [443, 80]
`

func TestProxyExposureApplyGetListAndDelete(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "proxy-exposure", "apply", "-f", writeGroup(t, proxyExposureYAML))...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "proxy exposure web applied: address 192.168.1.81, ports 443,80, pending")
	out, err = run(t, op.args(f, "proxy-exposure", "get", "web")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"host": "h1"`)
	out, err = run(t, op.args(f, "proxy-exposure", "list")...)
	require.NoError(t, err, out)
	assert.Regexp(t, `web\s+edge\s+192\.168\.1\.80\s+443,80\s+h1\s+announced`, out)
	out, err = run(t, op.args(f, "proxy-exposure", "delete", "web")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "proxy exposure web is being released")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, orchapi.ProxyExposureSpec{Pool: "edge", RequestedAddress: "192.168.1.81", Ports: []int{443, 80}}, f.proxies["web"])
	assert.Equal(t, []string{"web"}, f.releasedProxies)
}

func TestProxyExposureApply_RefusesAFileThatIsNotAProxyExposure(t *testing.T) {
	for name, body := range map[string]string{
		"a field proxy exposures do not have": proxyExposureYAML + "backends: [10.9.0.5]\n",
		"no id":                               strings.Replace(proxyExposureYAML, "id: web\n", "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOrch(t)
			op := newOperator(t, f, "operator")
			_, err := run(t, op.args(f, "proxy-exposure", "apply", "-f", writeGroup(t, body))...)
			require.Error(t, err)
			f.mu.Lock()
			defer f.mu.Unlock()
			assert.Empty(t, f.proxies)
		})
	}
}
