package pki_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/naructl/internal/pki"
)

func TestServiceAccountKey_PublicKeyIsTheKeysOwn(t *testing.T) {
	keyPEM, pubPEM, err := pki.ServiceAccountKey()
	require.NoError(t, err)

	keyBlock, _ := pem.Decode(keyPEM)
	require.NotNil(t, keyBlock)
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	require.NoError(t, err)
	pubBlock, _ := pem.Decode(pubPEM)
	require.NotNil(t, pubBlock)
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	require.NoError(t, err)
	assert.True(t, key.PublicKey.Equal(pub.(*ecdsa.PublicKey)))
}

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
