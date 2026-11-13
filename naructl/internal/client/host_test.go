package client_test

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/naructl/internal/client"
	"github.com/garamsh/naru/naructl/internal/pki"
)

func organizations(t *testing.T, f client.File) []string {
	t.Helper()
	b, _ := pem.Decode([]byte(f.Certificate))
	require.NotNil(t, b)
	c, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return c.Subject.Organization
}

func TestIssueHost_TheRoleIsTheOrganizationAndThePortDefaultsTo7443(t *testing.T) {
	ca, err := pki.New("host")
	require.NoError(t, err)

	for role, org := range map[string]string{"provisioner": "naru:host-provisioner", "reader": "naru:host-reader"} {
		f, err := client.IssueHost(ca, role, "ops", []string{"192.168.0.10", "192.168.0.11:9000"})
		require.NoError(t, err)
		assert.Equal(t, []string{org}, organizations(t, f))
		assert.Equal(t, []string{"192.168.0.10:7443", "192.168.0.11:9000"}, f.Endpoints)
	}

	_, err = client.IssueHost(ca, "admin", "ops", []string{"192.168.0.10"})
	assert.Error(t, err, "a Node API role is not a host role")
	_, err = client.IssueHost(ca, "reader", "ops", nil)
	assert.Error(t, err)
	_, err = client.IssueHost(ca, "reader", "ops", []string{"node.example"})
	assert.Error(t, err, "a name is refused, as the serving certificate names IP addresses")
}

func TestAddressPort_TheNodeAPIKeepsItsDefault(t *testing.T) {
	a, err := client.Address("192.168.0.10")
	require.NoError(t, err)
	assert.Equal(t, "192.168.0.10:50000", a)
	a, err = client.AddressPort("192.168.0.10", "7443")
	require.NoError(t, err)
	assert.Equal(t, "192.168.0.10:7443", a)
}
