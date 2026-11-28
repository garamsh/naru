package renewal_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/renewal"
	"github.com/garamsh/naru/orch/internal/signer"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k
}

func pemOf(certs ...*x509.Certificate) []byte {
	var out []byte
	for _, c := range certs {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

func parse(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}

// authority is the signer's orch authority: its key, and every certificate of it signed so far.
type authority struct {
	key   *ecdsa.PrivateKey
	certs []*x509.Certificate
}

func newAuthority(t *testing.T, start time.Time) *authority {
	a := &authority{key: newKey(t)}
	a.resign(t, start, start.AddDate(10, 0, 0))
	return a
}

// resign signs the authority again under its key, as the signer does two years before it ends.
func (a *authority) resign(t *testing.T, notBefore, notAfter time.Time) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(int64(len(a.certs) + 1)), Subject: pkix.Name{CommonName: "naru-orch-ca"},
		NotBefore: notBefore, NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &a.key.PublicKey, a.key)
	require.NoError(t, err)
	c := parse(t, der)
	a.certs = append(a.certs, c)
	return c
}

var serial int64 = 100

// issue is a member certificate for pub, as the signer issues orch-1's.
func (a *authority) issue(t *testing.T, cn string, pub *ecdsa.PublicKey, notBefore, notAfter time.Time) *x509.Certificate {
	t.Helper()
	serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn, OrganizationalUnit: []string{string(identity.RoleOrch)}},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, DNSNames: []string{cn + ".example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.certs[0], pub, a.key)
	require.NoError(t, err)
	return parse(t, der)
}

// fakeSigner verifies a renewal and a retire as naru-signer does, against the authority, and
// issues the renewal for the key the request is of.
type fakeSigner struct {
	t     *testing.T
	auth  *authority
	now   func() time.Time
	mu    sync.Mutex
	renew error
	// otherKey, when set, issues the renewal for another key.
	otherKey  bool
	retire    error
	forged    *x509.Certificate
	renewed   []string
	retiredBy []string
}

func (s *fakeSigner) roots() *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range s.auth.certs {
		p.AddCert(c)
	}
	return p
}

func (s *fakeSigner) RenewOrch(_ context.Context, signed []byte) (identity.Issued, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renew != nil {
		return identity.Issued{}, s.renew
	}
	who, body, err := identity.VerifyRequest(signed, identity.ActionRenewOrch, s.roots(), &identity.CRL{}, s.now())
	if err != nil {
		return identity.Issued{}, err
	}
	var r identity.OrchRenewal
	if err := json.Unmarshal(body, &r); err != nil {
		return identity.Issued{}, err
	}
	csr, err := x509.ParseCertificateRequest(r.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return identity.Issued{}, errors.New("not an ECDSA key")
	}
	if s.otherKey {
		pub = &newKey(s.t).PublicKey
	}
	c := s.auth.issue(s.t, who.Subject, pub, s.now(), s.now().AddDate(1, 0, 0))
	s.renewed = append(s.renewed, who.Serial)
	return identity.Issued{Certificate: c.Raw, Serial: identity.SerialOf(c)}, nil
}

func (s *fakeSigner) RetireOrch(_ context.Context, signed []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retire != nil {
		return s.retire
	}
	who, _, err := identity.VerifyRequest(signed, identity.ActionRetireOwnOrch, s.roots(), &identity.CRL{}, s.now())
	if err != nil {
		return err
	}
	s.retiredBy = append(s.retiredBy, who.Serial)
	return nil
}

// Authorities is the signer's GET /v1/ca: the orch authority's latest certificate, and forged
// in the place of the host authority's when it is set.
func (s *fakeSigner) Authorities(context.Context) (identity.Authorities, error) {
	a := identity.Authorities{Orch: s.auth.certs[len(s.auth.certs)-1].Raw}
	if s.forged != nil {
		a.Host = s.forged.Raw
	}
	return a, nil
}

// revocations revokes the serials it holds, or cannot be had when down.
type revocations struct {
	serials map[string]bool
	down    bool
}

func (r *revocations) Check(_ context.Context, chains [][]*x509.Certificate) error {
	if r.down {
		return errors.New("the signer is down")
	}
	for _, c := range chains[0] {
		if r.serials[identity.SerialOf(c)] {
			return fmt.Errorf("%w: serial %s", identity.ErrRevoked, identity.SerialOf(c))
		}
	}
	return nil
}

type rig struct {
	t      *testing.T
	now    time.Time
	auth   *authority
	key    *ecdsa.PrivateKey
	first  *x509.Certificate
	signer *fakeSigner
	revs   *revocations
	cfg    renewal.Config
}

func newRig(t *testing.T) *rig {
	start := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	r := &rig{t: t, now: start.Add(time.Hour), auth: newAuthority(t, start), key: newKey(t), revs: &revocations{serials: map[string]bool{}}}
	r.first = r.auth.issue(t, "orch-1", &r.key.PublicKey, start, start.AddDate(1, 0, 0))
	r.signer = &fakeSigner{t: t, auth: r.auth, now: r.clock}
	dir := t.TempDir()
	r.cfg = renewal.Config{
		FirstCert: filepath.Join(dir, "run", "orch.crt"), Key: filepath.Join(dir, "run", "orch.key"),
		CertFile: filepath.Join(dir, "data", "tls", "orch.crt"), CA: filepath.Join(dir, "run", "ca.crt"),
		Store: filepath.Join(dir, "data", "tls", "ca-store.pem"), EtcdCA: filepath.Join(dir, "data", "tls", "etcd-ca.pem"),
		Signer: r.signer, Revocations: r.revs, Now: r.clock, Log: quiet,
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(r.cfg.FirstCert), 0o700))
	der, err := x509.MarshalPKCS8PrivateKey(r.key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(r.cfg.Key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(r.cfg.FirstCert, pemOf(r.first, r.auth.certs[0]), 0o600))
	require.NoError(t, os.WriteFile(r.cfg.CA, pemOf(r.auth.certs[0]), 0o644))
	return r
}

func (r *rig) clock() time.Time { return r.now }

// presented is the leaf CertFile holds, and the certificates after it.
func (r *rig) presented() (*x509.Certificate, []*x509.Certificate) {
	r.t.Helper()
	pair, err := tls.LoadX509KeyPair(r.cfg.CertFile, r.cfg.Key)
	require.NoError(r.t, err)
	var rest []*x509.Certificate
	for _, der := range pair.Certificate[1:] {
		rest = append(rest, parse(r.t, der))
	}
	return pair.Leaf, rest
}

// renewed is a certificate renewed for the member's key, as CertFile holds it.
func (r *rig) renewed(notBefore time.Time) *x509.Certificate {
	r.t.Helper()
	c := r.auth.issue(r.t, "orch-1", &r.key.PublicKey, notBefore, notBefore.AddDate(1, 0, 0))
	require.NoError(r.t, os.WriteFile(r.cfg.CertFile, pemOf(c, r.auth.certs[0]), 0o600))
	return c
}

func (r *rig) due() { r.now = r.first.NotAfter.Add(-identity.OrchRenewWindow).Add(time.Hour) }

func TestPrepareWritesTheFirstPairWhereNoRenewedOneIsAndEtcdsAuthoritiesAsTheyStandAtStart(t *testing.T) {
	r := newRig(t)
	require.NoError(t, renewal.Prepare(t.Context(), r.cfg))
	leaf, rest := r.presented()
	assert.Equal(t, r.first.Raw, leaf.Raw, "the first pair, at the first start")
	assert.Equal(t, []*x509.Certificate{r.auth.certs[0]}, rest, "with its authority's certificate")
	fi, err := os.Stat(r.cfg.CertFile)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	resigned := r.auth.resign(t, r.now, r.now.AddDate(10, 0, 0))
	require.NoError(t, os.WriteFile(r.cfg.Store, pemOf(resigned), 0o644))
	require.NoError(t, renewal.Prepare(t.Context(), r.cfg))
	b, err := os.ReadFile(r.cfg.EtcdCA)
	require.NoError(t, err)
	assert.Equal(t, append(append(pemOf(r.auth.certs[0]), '\n'), pemOf(resigned)...), b, "--ca and the store")

	same := r.cfg
	same.CertFile = same.FirstCert
	require.Error(t, renewal.Prepare(t.Context(), same), "init overwrites --cert at each boot")
}

func TestPrepareKeepsARenewedCertificateOnlyWhenItIsAGoodPairThatEndsAfterTheFirst(t *testing.T) {
	cases := []struct {
		name string
		make func(r *rig) []byte
		keep bool
	}{
		{"a renewed pair", func(r *rig) []byte { return pemOf(r.renewed(r.now)) }, true},
		{"the list cannot be had", func(r *rig) []byte { r.revs.down = true; return pemOf(r.renewed(r.now)) }, true},
		{"a torn write", func(r *rig) []byte {
			b := pemOf(r.renewed(r.now))
			return b[:len(b)/2]
		}, false},
		{"another key", func(r *rig) []byte {
			return pemOf(r.auth.issue(r.t, "orch-1", &newKey(r.t).PublicKey, r.now, r.now.AddDate(1, 0, 0)))
		}, false},
		{"revoked", func(r *rig) []byte {
			c := r.renewed(r.now)
			r.revs.serials[identity.SerialOf(c)] = true
			return pemOf(c)
		}, false},
		{"ended", func(r *rig) []byte {
			c := r.renewed(r.now)
			r.now = c.NotAfter.Add(time.Hour)
			require.NoError(r.t, os.WriteFile(r.cfg.FirstCert, pemOf(r.auth.issue(r.t, "orch-1", &r.key.PublicKey, r.first.NotBefore, r.now.AddDate(0, 0, -1)), r.auth.certs[0]), 0o600))
			return pemOf(c)
		}, false},
		{"no later than the first", func(r *rig) []byte {
			return pemOf(r.auth.issue(r.t, "orch-1", &r.key.PublicKey, r.first.NotBefore, r.first.NotAfter))
		}, false},
		{"under another authority", func(r *rig) []byte {
			return pemOf(newAuthority(r.t, r.first.NotBefore).issue(r.t, "orch-1", &r.key.PublicKey, r.now, r.now.AddDate(1, 0, 0)))
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			require.NoError(t, os.MkdirAll(filepath.Dir(r.cfg.CertFile), 0o700))
			held := tc.make(r)
			require.NoError(t, os.WriteFile(r.cfg.CertFile, held, 0o600))
			first, err := os.ReadFile(r.cfg.FirstCert)
			require.NoError(t, err)
			require.NoError(t, renewal.Prepare(t.Context(), r.cfg))
			got, err := os.ReadFile(r.cfg.CertFile)
			require.NoError(t, err)
			if tc.keep {
				assert.Equal(t, held, got, "the renewed certificate is kept")
			} else {
				assert.Equal(t, first, got, "the first pair replaces it")
			}
		})
	}
}

func TestAMemberRenewsInItsLastNinetyDaysThenPresentsTheRenewalAndRetiresItsOthers(t *testing.T) {
	r := newRig(t)
	require.NoError(t, renewal.Prepare(t.Context(), r.cfg))
	w := renewal.New(r.cfg)

	require.NoError(t, w.Check(t.Context()))
	assert.Empty(t, r.signer.renewed, "a year left is not due")

	r.due()
	resigned := r.auth.resign(t, r.now, r.now.AddDate(10, 0, 0))
	require.NoError(t, w.Check(t.Context()))
	require.Equal(t, []string{identity.SerialOf(r.first)}, r.signer.renewed, "control: due, signed by the first certificate")
	leaf, rest := r.presented()
	assert.True(t, leaf.NotAfter.After(r.first.NotAfter), "the renewed certificate is what every reader presents")
	assert.Equal(t, []*x509.Certificate{resigned}, rest, "with the authority's certificate that ends last, the store's re-signing")
	assert.Equal(t, []string{identity.SerialOf(leaf)}, r.signer.retiredBy, "the renewed certificate retires the others")
	assert.NoFileExists(t, retirePath(r))

	require.NoError(t, w.Check(t.Context()))
	assert.Len(t, r.signer.renewed, 1, "the renewed certificate is not due")
	assert.Len(t, r.signer.retiredBy, 1, "and its retire is done")
}

func retirePath(r *rig) string { return filepath.Join(filepath.Dir(r.cfg.CertFile), "orch.retire") }

func TestARetireThatFailsIsKeptAndTriedAtEachCheckAndOneForAnUnknownSerialIsDropped(t *testing.T) {
	r := newRig(t)
	require.NoError(t, renewal.Prepare(t.Context(), r.cfg))
	w := renewal.New(r.cfg)
	r.due()
	r.signer.retire = errors.New("the signer is down")
	require.Error(t, w.Check(t.Context()))
	leaf, _ := r.presented()
	assert.True(t, leaf.NotAfter.After(r.first.NotAfter), "a failed retire does not undo the renewal")
	assert.FileExists(t, retirePath(r), "the retire is kept")

	require.Error(t, w.Check(t.Context()), "tried again at the next check")
	r.signer.retire = nil
	require.NoError(t, w.Check(t.Context()))
	assert.Equal(t, []string{identity.SerialOf(leaf)}, r.signer.retiredBy)
	assert.NoFileExists(t, retirePath(r))

	require.NoError(t, os.WriteFile(retirePath(r), []byte(identity.SerialOf(leaf)+"\n"), 0o600))
	r.signer.retire = fmt.Errorf("%w: serial %s is not one of orch-1's", signer.ErrUnknownHost, identity.SerialOf(leaf))
	require.NoError(t, w.Check(t.Context()))
	assert.NoFileExists(t, retirePath(r), "a serial the signer holds no record of is dropped")

	require.NoError(t, os.WriteFile(retirePath(r), []byte("0abc\n"), 0o600))
	r.signer.retire = nil
	require.NoError(t, w.Check(t.Context()))
	assert.NoFileExists(t, retirePath(r), "one kept for a serial the member does not present is dropped")
	assert.Len(t, r.signer.retiredBy, 1, "without a retire")
}

func TestARenewalTheMemberCannotUseChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		breaks func(r *rig)
		fails  bool
	}{
		{"not due by the signer's clock", func(r *rig) {
			r.signer.renew = fmt.Errorf("%w: renew from then", identity.ErrOrchNotDue)
		}, false},
		{"another refusal", func(r *rig) { r.signer.renew = errors.New("the signer answered 409: a conflict") }, true},
		{"a certificate for another key", func(r *rig) { r.signer.otherKey = true }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			require.NoError(t, renewal.Prepare(t.Context(), r.cfg))
			before, err := os.ReadFile(r.cfg.CertFile)
			require.NoError(t, err)
			r.due()
			tc.breaks(r)
			if err := renewal.New(r.cfg).Check(t.Context()); tc.fails {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			after, err := os.ReadFile(r.cfg.CertFile)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.NoFileExists(t, retirePath(r), "and keeps no retire")
		})
	}
}

func TestAMemberStoresOnlyAReSigningOfAnAuthorityItTrusts(t *testing.T) {
	r := newRig(t)
	require.NoError(t, renewal.Prepare(t.Context(), r.cfg))
	w := renewal.New(r.cfg)
	require.NoError(t, w.RefreshTrust(t.Context()))
	assert.NoFileExists(t, r.cfg.Store, "nothing new is offered")

	resigned := r.auth.resign(t, r.now, r.now.AddDate(10, 0, 0))
	r.signer.forged = newAuthority(t, r.now).certs[0] // the authority's name under another key
	require.NoError(t, w.RefreshTrust(t.Context()))
	b, err := os.ReadFile(r.cfg.Store)
	require.NoError(t, err)
	assert.Equal(t, pemOf(resigned), b, "the re-signing, and not the certificate of another key")
	fi, err := os.Stat(r.cfg.Store)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())

	require.NoError(t, w.RefreshTrust(t.Context()))
	b2, err := os.ReadFile(r.cfg.Store)
	require.NoError(t, err)
	assert.Equal(t, b, b2, "a re-signing already stored is not stored again")
}
