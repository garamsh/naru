package identity_test

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// signedAs is a CA certificate of subject cn for pub, issued under the name issuer and signed by key.
func signedAs(t *testing.T, cn string, pub *ecdsa.PublicKey, issuer string, key *ecdsa.PrivateKey, notBefore, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(notAfter.UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	parent := &x509.Certificate{Subject: pkix.Name{CommonName: issuer}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
	require.NoError(t, err)
	return der
}

func TestResignsTakesOnlyALaterReSigningAnAuthoritysOwnKeySignedWithinTenYears(t *testing.T) {
	now := time.Now()
	key, other := p256(t), p256(t)
	held := selfSigned(t, key, "naru-host-ca", now.AddDate(0, 0, 30), true)
	block, _ := pem.Decode(held)
	resign := func(cn string, pub *ecdsa.PublicKey, issuer string, signer *ecdsa.PrivateKey, life time.Duration) []byte {
		return signedAs(t, cn, pub, issuer, signer, now.Add(-time.Hour), now.Add(-time.Hour).Add(life))
	}
	tenYears := now.AddDate(10, 0, 0).Sub(now)

	for name, der := range map[string][]byte{
		"another key signed it":                  resign("naru-host-ca", &key.PublicKey, "naru-host-ca", other, tenYears),
		"issued under another name":              resign("naru-host-ca", &key.PublicKey, "naru-operator-ca", key, tenYears),
		"another key under the authority's name": resign("naru-host-ca", &other.PublicKey, "naru-host-ca", other, tenYears),
		"the key under another name":             resign("naru-operator-ca", &key.PublicKey, "naru-operator-ca", key, tenYears),
		"a century":                              resign("naru-host-ca", &key.PublicKey, "naru-host-ca", key, 100*tenYears/10),
		"ending no later":                        resign("naru-host-ca", &key.PublicKey, "naru-host-ca", key, 29*24*time.Hour),
		"the certificate held":                   block.Bytes,
		"not a certificate":                      []byte("junk"),
	} {
		got, took := identity.Resignings(held, der)
		assert.Empty(t, got, name)
		assert.Empty(t, took, name)
	}
	leaf := x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "naru-host-ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(5, 0, 0), BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, &leaf, &leaf, &key.PublicKey, key)
	require.NoError(t, err)
	got, _ := identity.Resignings(held, der)
	assert.Empty(t, got, "not an authority's certificate")

	genuine := resign("naru-host-ca", &key.PublicKey, "naru-host-ca", key, tenYears)
	got, took := identity.Resignings(held, genuine, genuine)
	require.Len(t, took, 1, "control: the authority's own re-signing for ten years, taken once")
	assert.Equal(t, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: genuine}), got)
}
