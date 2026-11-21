package pki_test

import (
	"encoding/base64"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/layout/pki"
)

func TestKubeconfig_CarriesTheServerTheAuthorityAndThePairInline(t *testing.T) {
	ca, err := pki.New("test")
	require.NoError(t, err)
	pair, err := ca.Client(pki.AdminName, pki.AdminOrganization)
	require.NoError(t, err)

	conf := string(pki.Kubeconfig(pki.AdminName, "https://192.168.0.10:6443", ca.CertPEM, pair))

	assert.Contains(t, conf, "server: https://192.168.0.10:6443")
	assert.Contains(t, conf, "current-context: kubernetes-admin@naru")
	for _, field := range []struct {
		name string
		want []byte
	}{{"certificate-authority-data", ca.CertPEM}, {"client-certificate-data", pair.CertPEM}, {"client-key-data", pair.KeyPEM}} {
		m := regexp.MustCompile(field.name + `: (\S+)`).FindStringSubmatch(conf)
		require.Len(t, m, 2, field.name)
		got, err := base64.StdEncoding.DecodeString(m[1])
		require.NoError(t, err)
		assert.Equal(t, field.want, got, field.name)
	}
}
