package signer

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

func recoveryPublic(t *testing.T) identity.RecoveryPublic {
	t.Helper()
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	raw, err := rec.Public().MarshalPublic()
	require.NoError(t, err)
	pub, err := identity.ParseRecoveryPublic(raw)
	require.NoError(t, err)
	return pub
}

func hostLeaf(t *testing.T, a *authorities, now time.Time) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	issued, err := issueLeaf(a.host, now, "host-1", identity.RoleHost, &key.PublicKey, serverAndClient, &x509.CertificateRequest{DNSNames: []string{"host-1"}})
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(issued.Certificate)
	require.NoError(t, err)
	return cert
}

func verifiesAt(leaf *x509.Certificate, at time.Time, roots ...*x509.Certificate) error {
	p := x509.NewCertPool()
	for _, r := range roots {
		p.AddCert(r)
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: p, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func TestAnAuthorityIsSignedAgainUnderItsKeyOnceTwoOfItsTenYearsRemain(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	a, err := newAuthorities(recoveryPublic(t), t0)
	require.NoError(t, err)

	due := identity.RenewsBy(a.host.cert)
	require.Equal(t, t0.AddDate(identity.AuthorityYears-identity.AuthorityRenewYears, 0, 0), due)

	same, names, err := a.renewed(due.Add(-time.Second))
	require.NoError(t, err)
	assert.Empty(t, names, "more than two years remain: nothing is signed again")
	assert.Same(t, a, same)

	now := due.Add(time.Hour)
	before := hostLeaf(t, a, now)
	next, names, err := a.renewed(now)
	require.NoError(t, err)
	assert.Equal(t, []string{"host", "operator", "orch"}, names)
	for name, pair := range map[string][2]authority{
		"host": {a.host, next.host}, "operator": {a.operator, next.operator}, "orch": {a.orch, next.orch},
	} {
		old, fresh := pair[0], pair[1]
		assert.Same(t, old.key, fresh.key, "%s keeps its key", name)
		assert.Equal(t, old.cert.RawSubject, fresh.cert.RawSubject, "%s keeps its subject", name)
		assert.Equal(t, old.cert.RawSubjectPublicKeyInfo, fresh.cert.RawSubjectPublicKeyInfo, name)
		assert.NotEqual(t, old.cert.SerialNumber, fresh.cert.SerialNumber, name)
		assert.Equal(t, now.AddDate(identity.AuthorityYears, 0, 0), fresh.cert.NotAfter, "%s lives ten years from now", name)
		assert.True(t, fresh.cert.IsCA, name)
		assert.NoError(t, fresh.cert.CheckSignatureFrom(fresh.cert), "%s is self-signed by its own key", name)
		assert.Equal(t, old.cert.KeyUsage, fresh.cert.KeyUsage, name)
		assert.Equal(t, old.cert.SubjectKeyId, fresh.cert.SubjectKeyId, name)
	}
	require.NoError(t, next.check())

	require.NoError(t, verifiesAt(before, now.Add(time.Hour), next.host.cert), "a leaf the old certificate signed verifies against the new one")
	after := hostLeaf(t, next, now.Add(time.Hour))
	require.NoError(t, verifiesAt(after, now.Add(2*time.Hour), a.host.cert), "and one the new signed, against the old, within the old one's life")

	end := a.host.cert.NotAfter
	late := hostLeaf(t, next, end.Add(-24*time.Hour))
	require.Error(t, verifiesAt(late, end.Add(time.Hour), a.host.cert), "a pool that kept only the old certificate stops trusting at its end")
	require.NoError(t, verifiesAt(late, end.Add(time.Hour), a.host.cert, next.host.cert), "one that took the new keeps trusting")

	again, names, err := next.renewed(now.Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, names, "a renewed authority is not due again for eight years")
	assert.Same(t, next, again)
}

func TestRenewAuthoritiesKeepsTheNewCertificatesAndGETCAServesThem(t *testing.T) {
	t0 := time.Now().UTC().Truncate(time.Second)
	dir := filepath.Join(t.TempDir(), "signer")
	require.NoError(t, os.Mkdir(dir, 0o700))
	a, err := newAuthorities(recoveryPublic(t), t0)
	require.NoError(t, err)
	require.NoError(t, a.save(dir))

	clock := t0
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(Options{Dir: dir, Log: quiet, Now: func() time.Time { return clock }})
	require.NoError(t, err)
	names, err := s.RenewAuthorities()
	require.NoError(t, err)
	assert.Empty(t, names)

	clock = identity.RenewsBy(a.orch.cert).Add(time.Minute)
	names, err = s.RenewAuthorities()
	require.NoError(t, err)
	assert.Equal(t, []string{"host", "operator", "orch"}, names)
	served, err := s.Authorities()
	require.NoError(t, err)
	orch, err := x509.ParseCertificate(served.Orch)
	require.NoError(t, err)
	assert.Equal(t, clock.AddDate(identity.AuthorityYears, 0, 0), orch.NotAfter, "GET /v1/ca serves the new certificate")

	reopened, err := Open(Options{Dir: dir, Log: quiet})
	require.NoError(t, err)
	kept, err := reopened.Authorities()
	require.NoError(t, err)
	assert.Equal(t, served, kept, "the directory keeps the new certificates")
	onDisk, err := os.ReadFile(filepath.Join(dir, orchCAFile+certSuffix))
	require.NoError(t, err)
	block, _ := pem.Decode(onDisk)
	require.NotNil(t, block)
	assert.Equal(t, served.Orch, block.Bytes)

	plain, err := reopened.snapshot().marshalBundle()
	require.NoError(t, err)
	bundled, err := parseBundle(plain)
	require.NoError(t, err)
	assert.Equal(t, served.Host, bundled.host.cert.Raw, "the bundle and a replica carry the new certificates")
}
