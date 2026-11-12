package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func bootPKICfg(t *testing.T, index int) Config {
	t.Helper()
	hostPort := 0
	if index == 1 {
		hostPort = 6443
	}
	c := gaonCfg(t, index, hostPort)
	c.Gaon.BootPKI = true
	return c
}

func fileContents(n nodeConfig) map[string]string {
	m := map[string]string{}
	for _, f := range n.Spec.Files {
		m[f.Path] = f.Content
	}
	return m
}

func TestNaruYAML_BootPKIControlPlaneIssuesItsPairsFromTheCAAndCarriesNoPairAsAFile(t *testing.T) {
	c := bootPKICfg(t, 1)
	n := parse(t, c)

	require.NotNil(t, n.Spec.PKI)
	require.Len(t, n.Spec.PKI.Authorities, 1)
	ca := n.Spec.PKI.Authorities[0]
	assert.Equal(t, string(c.Gaon.Certs.CA), ca.Certificate)
	assert.Equal(t, string(c.Gaon.Certs.CAKey), ca.Key, "the CA's key is in the configuration")
	assert.Equal(t, "/run/gaon/ca.pem", ca.CertificatePath)

	issued := map[string]pkiCertificate{}
	for _, cert := range n.Spec.PKI.Certificates {
		issued[cert.Name] = cert
		assert.Equal(t, "gaon", cert.Authority)
		assert.Equal(t, "/run/gaon/"+cert.Name+".pem", cert.CertificatePath)
		assert.Equal(t, "/run/gaon/"+cert.Name+".key", cert.KeyPath)
	}
	require.Len(t, issued, 4)
	assert.Equal(t, "server", issued["apiserver"].Usage)
	assert.True(t, issued["apiserver"].NodeAddresses, "the serving certificate names the Node's addresses")
	assert.Equal(t, "node-1", issued["agent"].CommonName, "the agent's CN is its Node's name")
	for name, service := range map[string]string{
		"apiserver": "gaon-apiserver", "scheduler": "gaon-scheduler", "controllers": "gaon-controllers", "agent": "gaon-node-agent",
	} {
		assert.Equal(t, []string{service}, issued[name].Restart, name)
		serviceNamed(t, n, service)
	}

	for path, content := range fileContents(n) {
		assert.NotContains(t, path, "/run/gaon/", "no certificate or key is carried as a file")
		assert.NotContains(t, content, "PRIVATE KEY", "no key is carried as a file: "+path)
	}
}

func TestNaruYAML_BootPKIWorkerKeepsItsAgentPairAsFilesAndHoldsNoCAKey(t *testing.T) {
	c := bootPKICfg(t, 2)
	n := parse(t, c)

	assert.Nil(t, n.Spec.PKI)
	files := fileContents(n)
	for _, p := range []string{"/run/gaon/ca.pem", "/run/gaon/agent.pem", "/run/gaon/agent.key"} {
		assert.Contains(t, files, p)
	}
	for _, content := range files {
		assert.NotContains(t, content, string(c.Gaon.Certs.CAKey), "the worker never holds the CA's key")
	}
}

func TestNaruYAML_WithoutBootPKINoNodeCarriesSpecPKIAndTheControlPlaneCarriesItsPairs(t *testing.T) {
	for _, i := range []int{1, 2} {
		hostPort := 0
		if i == 1 {
			hostPort = 6443
		}
		n := parse(t, gaonCfg(t, i, hostPort))
		assert.Nil(t, n.Spec.PKI, "node-%d carries its pairs as files, as in v0.2.0", i)
		assert.Contains(t, fileContents(n), "/run/gaon/agent.key")
	}
	assert.Contains(t, fileContents(parse(t, gaonCfg(t, 1, 6443))), "/run/gaon/apiserver.key")
}
