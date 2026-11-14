package identity_test

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newCA(t *testing.T) ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return ca{cert: cert, key: key, pool: pool}
}

func (c ca) issue(t *testing.T, cn string, serial int64, notAfter time.Time, units ...string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: units},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert, key
}

func TestRoleOf(t *testing.T) {
	c := newCA(t)
	tests := []struct {
		name    string
		units   []string
		want    identity.Role
		wantErr bool
	}{
		{"operator", []string{"operator"}, identity.RoleOperator, false},
		{"host", []string{"host"}, identity.RoleHost, false},
		{"none", nil, "", true},
		{"two", []string{"operator", "root"}, "", true},
		{"unknown", []string{"admin"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert, _ := c.issue(t, "x", 10, now.Add(time.Hour), tt.units...)
			got, err := identity.RoleOf(cert)
			if tt.wantErr {
				assert.ErrorIs(t, err, identity.ErrRole)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAuthenticate(t *testing.T) {
	c := newCA(t)
	cert, _ := c.issue(t, "alice", 0xab, now.Add(time.Hour), "operator")
	chains := [][]*x509.Certificate{{cert, c.cert}}

	id, err := identity.Authenticate(chains, nil)
	require.NoError(t, err)
	assert.Equal(t, identity.Identity{Subject: "alice", Role: identity.RoleOperator, Serial: "ab"}, id)

	_, err = identity.Authenticate(chains, &identity.CRL{Serials: []string{"ab"}})
	assert.ErrorIs(t, err, identity.ErrRevoked)

	_, err = identity.Authenticate(chains, &identity.CRL{Serials: []string{"cd"}})
	assert.NoError(t, err)

	_, err = identity.Authenticate(nil, nil)
	assert.ErrorIs(t, err, identity.ErrNoChain)
}

func TestCRL(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signed, err := identity.SignCRL(priv, identity.CRL{Seq: 5, IssuedAt: now, Serials: []string{"1f"}})
	require.NoError(t, err)

	crl, err := identity.VerifyCRL(pub, signed)
	require.NoError(t, err)
	assert.Equal(t, uint64(5), crl.Seq)
	assert.True(t, crl.Revokes("1f"))
	assert.False(t, crl.Revokes("20"))

	t.Run("another key", func(t *testing.T) {
		other, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		_, err = identity.VerifyCRL(other, signed)
		assert.ErrorIs(t, err, identity.ErrCRLSignature)
	})
	t.Run("tampered", func(t *testing.T) {
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(signed, &raw))
		body := strings.Replace(string(raw["crl"]), `"1f"`, `"20"`, 1)
		raw["crl"] = json.RawMessage(body)
		tampered, err := json.Marshal(raw)
		require.NoError(t, err)
		_, err = identity.VerifyCRL(pub, tampered)
		assert.ErrorIs(t, err, identity.ErrCRLSignature)
	})
	t.Run("not json", func(t *testing.T) {
		_, err := identity.VerifyCRL(pub, []byte("x"))
		assert.Error(t, err)
	})
	t.Run("sequence", func(t *testing.T) {
		assert.NoError(t, crl.CheckSeq(5))
		assert.NoError(t, crl.CheckSeq(4))
		assert.ErrorIs(t, crl.CheckSeq(6), identity.ErrCRLStale)
	})
	t.Run("nil list revokes nothing", func(t *testing.T) {
		var none *identity.CRL
		assert.False(t, none.Revokes("1f"))
	})
}

func TestPairingCode(t *testing.T) {
	a, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	b, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	aDER, err := x509.MarshalPKIXPublicKey(&a.PublicKey)
	require.NoError(t, err)
	bDER, err := x509.MarshalPKIXPublicKey(&b.PublicKey)
	require.NoError(t, err)

	code := identity.PairingCode(aDER)
	assert.Regexp(t, `^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`, code)
	assert.Equal(t, code, identity.PairingCode(aDER))
	assert.NotEqual(t, code, identity.PairingCode(bDER))

	// SHA-256 of "abc" begins ba7816bf8f, which is the 5-bit groups
	// 23 9 28 1 13 15 28 15, and Crockford spells those Q 9 W 1 D F W F.
	assert.Equal(t, "Q9W1-DFWF", identity.PairingCode([]byte("abc")))
}

func admission(host string, expires time.Time) identity.Admission {
	return identity.Admission{
		HostID: host, HostKeySHA256: strings.Repeat("ab", 32), Region: "kr-1",
		Labels: map[string]string{"rack": "r1"}, ExpiresAt: expires,
	}
}

func TestAdmission(t *testing.T) {
	c := newCA(t)
	sign := func(t *testing.T, role string, serial int64, a identity.Admission) []byte {
		cert, key := c.issue(t, "signer", serial, now.Add(24*time.Hour), role)
		stmt, err := identity.SignAdmission(a, key, cert)
		require.NoError(t, err)
		return stmt
	}
	valid := admission("host-1", now.Add(time.Hour))

	t.Run("operator", func(t *testing.T) {
		id, a, err := identity.VerifyAdmission(sign(t, "operator", 20, valid), c.pool, nil, now)
		require.NoError(t, err)
		assert.Equal(t, identity.RoleOperator, id.Role)
		assert.Equal(t, valid, a)
	})
	t.Run("root", func(t *testing.T) {
		_, _, err := identity.VerifyAdmission(sign(t, "root", 21, valid), c.pool, nil, now)
		assert.NoError(t, err)
	})
	t.Run("reader is refused", func(t *testing.T) {
		_, _, err := identity.VerifyAdmission(sign(t, "reader", 22, valid), c.pool, nil, now)
		assert.ErrorIs(t, err, identity.ErrAdmissionRole)
	})
	t.Run("automation is refused", func(t *testing.T) {
		_, _, err := identity.VerifyAdmission(sign(t, "automation", 23, valid), c.pool, nil, now)
		assert.ErrorIs(t, err, identity.ErrAdmissionRole)
	})
	t.Run("revoked operator is refused", func(t *testing.T) {
		_, _, err := identity.VerifyAdmission(sign(t, "operator", 0x24, valid), c.pool, &identity.CRL{Serials: []string{"24"}}, now)
		assert.ErrorIs(t, err, identity.ErrRevoked)
	})
	t.Run("expired statement is refused", func(t *testing.T) {
		_, _, err := identity.VerifyAdmission(sign(t, "operator", 25, valid), c.pool, nil, now.Add(2*time.Hour))
		assert.ErrorIs(t, err, identity.ErrAdmissionExpired)
	})
	t.Run("certificate of another authority is refused", func(t *testing.T) {
		other := newCA(t)
		_, _, err := identity.VerifyAdmission(sign(t, "operator", 26, valid), other.pool, nil, now)
		assert.Error(t, err)
	})
	t.Run("tampered admission is refused", func(t *testing.T) {
		stmt := sign(t, "operator", 27, valid)
		var raw map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(stmt, &raw))
		raw["admission"] = json.RawMessage(strings.Replace(string(raw["admission"]), "host-1", "host-2", 1))
		tampered, err := json.Marshal(raw)
		require.NoError(t, err)
		_, _, err = identity.VerifyAdmission(tampered, c.pool, nil, now)
		assert.ErrorIs(t, err, identity.ErrAdmissionSignature)
	})
	t.Run("not json", func(t *testing.T) {
		_, _, err := identity.VerifyAdmission([]byte("x"), c.pool, nil, now)
		assert.Error(t, err)
	})
}

func TestPersonFile(t *testing.T) {
	c := newCA(t)
	cert, key := c.issue(t, "alice", 30, now.Add(time.Hour), "operator")
	file, err := identity.EncryptPerson("correct horse", identity.Person{Key: key, Cert: cert})
	require.NoError(t, err)
	assert.Contains(t, string(file), "-----BEGIN NARU PERSON FILE-----")
	assert.Contains(t, string(file), "Version: 1")

	p, err := identity.DecryptPerson("correct horse", file)
	require.NoError(t, err)
	assert.True(t, cert.Equal(p.Cert))
	assert.True(t, key.Equal(p.Key))

	t.Run("wrong passphrase", func(t *testing.T) {
		_, err := identity.DecryptPerson("wrong", file)
		assert.ErrorIs(t, err, identity.ErrPassphrase)
	})
	t.Run("tampered body", func(t *testing.T) {
		lines := strings.Split(string(file), "\n")
		for i, l := range lines {
			if !strings.Contains(l, ":") && !strings.HasPrefix(l, "-----") && len(l) > 4 {
				lines[i] = "AAAA" + l[4:]
				break
			}
		}
		_, err := identity.DecryptPerson("correct horse", []byte(strings.Join(lines, "\n")))
		assert.Error(t, err)
	})
	t.Run("another kind of file", func(t *testing.T) {
		rec, err := identity.GenerateRecovery()
		require.NoError(t, err)
		recFile, err := rec.Encrypt("correct horse")
		require.NoError(t, err)
		_, err = identity.DecryptPerson("correct horse", recFile)
		assert.ErrorIs(t, err, identity.ErrFormat)
	})
	t.Run("not a file", func(t *testing.T) {
		_, err := identity.DecryptPerson("x", []byte("nothing"))
		assert.ErrorIs(t, err, identity.ErrFormat)
	})
	t.Run("two encryptions differ", func(t *testing.T) {
		again, err := identity.EncryptPerson("correct horse", identity.Person{Key: key, Cert: cert})
		require.NoError(t, err)
		assert.NotEqual(t, file, again)
	})
}

func TestSealedBox(t *testing.T) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	sealed, err := identity.Seal(priv.PublicKey(), []byte("secret"), []byte("bundle"))
	require.NoError(t, err)
	assert.NotContains(t, string(sealed), "secret")

	got, err := identity.Open(priv, sealed, []byte("bundle"))
	require.NoError(t, err)
	assert.Equal(t, []byte("secret"), got)

	t.Run("another key", func(t *testing.T) {
		other, err := ecdh.X25519().GenerateKey(rand.Reader)
		require.NoError(t, err)
		_, err = identity.Open(other, sealed, []byte("bundle"))
		assert.ErrorIs(t, err, identity.ErrOpen)
	})
	t.Run("another info", func(t *testing.T) {
		_, err := identity.Open(priv, sealed, []byte("replicate"))
		assert.ErrorIs(t, err, identity.ErrOpen)
	})
	t.Run("tampered", func(t *testing.T) {
		bad := append([]byte(nil), sealed...)
		bad[len(bad)-1] ^= 1
		_, err := identity.Open(priv, bad, []byte("bundle"))
		assert.ErrorIs(t, err, identity.ErrOpen)
	})
	t.Run("short", func(t *testing.T) {
		_, err := identity.Open(priv, sealed[:10], []byte("bundle"))
		assert.ErrorIs(t, err, identity.ErrOpen)
		_, err = identity.Open(priv, sealed[:40], []byte("bundle"))
		assert.ErrorIs(t, err, identity.ErrOpen)
	})
	t.Run("two boxes differ", func(t *testing.T) {
		again, err := identity.Seal(priv.PublicKey(), []byte("secret"), []byte("bundle"))
		require.NoError(t, err)
		assert.NotEqual(t, sealed, again)
	})
}

func TestRecoveryKey(t *testing.T) {
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	file, err := rec.Encrypt("pass")
	require.NoError(t, err)

	got, err := identity.DecryptRecovery("pass", file)
	require.NoError(t, err)
	assert.True(t, rec.X25519.Equal(got.X25519))
	assert.True(t, rec.Ed25519.Equal(got.Ed25519))

	_, err = identity.DecryptRecovery("nope", file)
	assert.ErrorIs(t, err, identity.ErrPassphrase)

	t.Run("public halves export separately", func(t *testing.T) {
		data, err := rec.Public().MarshalPublic()
		require.NoError(t, err)
		pub, err := identity.ParseRecoveryPublic(data)
		require.NoError(t, err)
		assert.NotContains(t, string(data), string(rec.X25519.Bytes()))

		key, err := pub.SealKey()
		require.NoError(t, err)
		sealed, err := identity.Seal(key, []byte("backup"), []byte("bundle"))
		require.NoError(t, err)
		opened, err := identity.Open(got.X25519, sealed, []byte("bundle"))
		require.NoError(t, err)
		assert.Equal(t, []byte("backup"), opened)

		msg := []byte("m")
		assert.True(t, ed25519.Verify(pub.Ed25519, msg, ed25519.Sign(got.Ed25519, msg)))
	})
	t.Run("bad public keys", func(t *testing.T) {
		_, err := identity.ParseRecoveryPublic([]byte(`{"x25519":"AA==","ed25519":"AA=="}`))
		assert.ErrorIs(t, err, identity.ErrFormat)
		_, err = identity.ParseRecoveryPublic([]byte("x"))
		assert.Error(t, err)
	})
}
