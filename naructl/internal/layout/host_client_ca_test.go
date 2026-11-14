package layout_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/pki"
)

func TestUserData_HostTrustsOrchsAuthorityBesideTheHostAuthority(t *testing.T) {
	n, ca := hostNode(t)
	orchCA, err := pki.New("orch")
	require.NoError(t, err)
	n.Host.ClientCA = string(orchCA.CertPEM)

	data, err := n.UserData()
	require.NoError(t, err)

	var p parsed
	require.NoError(t, yaml.Unmarshal(data, &p))
	var bundle string
	for _, f := range p.Spec.Files {
		if f.Path == "/run/naru-hostd/client-ca.crt" {
			bundle = f.Content
		}
	}
	assert.Equal(t, 2, strings.Count(bundle, "BEGIN CERTIFICATE"), "the host authority's and orch's")
	assert.Contains(t, bundle, string(ca.CertPEM))
	assert.Contains(t, bundle, string(orchCA.CertPEM))
}

func TestCheck_RefusesAClientAuthorityThatIsNotACertificate(t *testing.T) {
	n, _ := hostNode(t)
	n.Host.ClientCA = "not a certificate"

	err := n.Check()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a PEM certificate")
}
