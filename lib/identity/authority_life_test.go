package identity_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
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

func selfSigned(t *testing.T, key *ecdsa.PrivateKey, cn string, notAfter time.Time, isCA bool) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: notAfter.AddDate(-10, 0, 0), NotAfter: notAfter, IsCA: isCA, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func p256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k
}

func TestTrustExpiryIsTheEarliestOfEachAuthoritysLatestCertificate(t *testing.T) {
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	host, orch := p256(t), p256(t)
	old := selfSigned(t, host, "naru-host-ca", t0, true)
	renewed := selfSigned(t, host, "naru-host-ca", t0.AddDate(8, 0, 0), true)
	orchCA := selfSigned(t, orch, "naru-orch-ca", t0.AddDate(3, 0, 0), true)
	other := selfSigned(t, p256(t), "naru-host-ca", t0.AddDate(-1, 0, 0), true) // another key under the same name
	leaf := selfSigned(t, p256(t), "not-a-ca", t0.AddDate(-5, 0, 0), false)

	_, ok := identity.TrustExpiry(nil)
	assert.False(t, ok, "an empty pool trusts no authority")
	_, ok = identity.TrustExpiry(leaf)
	assert.False(t, ok, "a certificate that is not a CA is no authority")

	got, ok := identity.TrustExpiry(old)
	require.True(t, ok)
	assert.Equal(t, identity.TrustEnd{At: t0, Authority: "naru-host-ca"}, got)

	got, _ = identity.TrustExpiry(append(append(append([]byte{}, old...), renewed...), leaf...))
	assert.Equal(t, t0.AddDate(8, 0, 0), got.At, "a pool that took the renewed certificate trusts the authority until its end")

	got, _ = identity.TrustExpiry(append(append(append([]byte{}, old...), renewed...), orchCA...))
	assert.Equal(t, identity.TrustEnd{At: t0.AddDate(3, 0, 0), Authority: "naru-orch-ca"}, got, "the authority that ends first, named")

	got, _ = identity.TrustExpiry(append(append([]byte{}, renewed...), other...))
	assert.Equal(t, t0.AddDate(-1, 0, 0), got.At, "a certificate of another key is another authority")
}

func TestTrustWarnsWithinAYearOfTheEnd(t *testing.T) {
	end := time.Date(2036, 11, 28, 0, 0, 0, 0, time.UTC)
	assert.False(t, identity.TrustWarns(end, end.AddDate(-1, 0, 0).Add(-time.Second)))
	assert.True(t, identity.TrustWarns(end, end.AddDate(-1, 0, 0)))
	assert.True(t, identity.TrustWarns(end, end.Add(time.Hour)), "past the end too")
	assert.True(t, identity.RenewsBy(&x509.Certificate{NotAfter: end}).Equal(end.AddDate(-2, 0, 0)))
}
