package enroll_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// signedByCaller verifies the request in r's body for action as orch and the signer do together:
// under the authority, signed by the certificate the caller connects with.
func (f *fakeOrch) signedByCaller(w http.ResponseWriter, r *http.Request, action string) (identity.Identity, []byte, bool) {
	body, _ := io.ReadAll(r.Body)
	pool := x509.NewCertPool()
	pool.AddCert(f.ca)
	who, raw, err := identity.VerifyRequest(body, action, pool, nil, time.Now())
	if err != nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || identity.SerialOf(r.TLS.PeerCertificates[0]) != who.Serial {
		f.json(w, http.StatusForbidden, orchapi.Error{Message: "not the caller's own request"})
		return identity.Identity{}, nil, false
	}
	return who, raw, true
}

func (f *fakeOrch) renew(w http.ResponseWriter, r *http.Request) {
	who, raw, ok := f.signedByCaller(w, r, identity.ActionRenewHost)
	if !ok {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.renewedUntil.IsZero() {
		// A test that renews nothing: the agent's first check, at its start, is to change nothing.
		f.json(w, http.StatusServiceUnavailable, orchapi.Error{Message: "this orch renews nothing"})
		return
	}
	if f.notDue {
		f.json(w, http.StatusConflict, orchapi.Error{Message: identity.ErrHostNotDue.Error()})
		return
	}
	var req identity.HostRenewal
	require.NoError(f.t, json.Unmarshal(raw, &req))
	csr, err := x509.ParseCertificateRequest(req.CSR)
	require.NoError(f.t, err)
	if !csr.PublicKey.(*ecdsa.PublicKey).Equal(who.PublicKey) {
		f.json(w, http.StatusBadRequest, orchapi.Error{Message: "a host renews under its own key"})
		return
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(20 + len(f.renewed))), Subject: pkix.Name{CommonName: who.Subject, OrganizationalUnit: []string{string(identity.RoleHost)}}, NotBefore: time.Now().Add(-time.Hour), NotAfter: f.renewedUntil, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.ca, csr.PublicKey, f.caKey)
	require.NoError(f.t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(f.t, err)
	f.renewed = append(f.renewed, cert)
	f.json(w, http.StatusOK, identity.Issued{Certificate: der, Serial: identity.SerialOf(cert), NotAfter: cert.NotAfter})
}

func (f *fakeOrch) retire(w http.ResponseWriter, r *http.Request) {
	who, _, ok := f.signedByCaller(w, r, identity.ActionRetireOwnHost)
	if !ok {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRetire {
		f.json(w, http.StatusServiceUnavailable, orchapi.Error{Message: "the signer is away"})
		return
	}
	serial, _ := new(big.Int).SetString(who.Serial, 16)
	f.retired = append(f.retired, serial.Int64())
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeOrch) authorities(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		f.json(w, http.StatusUnauthorized, orchapi.Error{Message: "a client certificate is required"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.json(w, http.StatusOK, f.offered)
}

// renewal is the state of a fake orch and an agent enrolled with it, whose clock the test moves.
type renewal struct {
	f      *fakeOrch
	a      *enroll.Agent
	dir    string
	served *served
	clock  *agentClock
	first  *x509.Certificate
}

type agentClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *agentClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.now.IsZero() {
		return time.Now()
	}
	return c.now
}

func (c *agentClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// served records the pairs the agent has the host API serve, and fail is what Serve answers.
type served struct {
	mu    sync.Mutex
	certs [][]byte
	fail  error
}

func (s *served) serve(certPEM, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.certs = append(s.certs, certPEM)
	return nil
}

func (s *served) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.certs)
}

// newRenewal enrolls a host whose first certificate lives a year, and whose renewed ones end in two.
func newRenewal(t *testing.T) *renewal {
	t.Helper()
	f := newFakeOrch(t, "naru-ek-secret")
	f.life, f.renewedUntil = 365*24*time.Hour, time.Now().AddDate(2, 0, 0)
	r := &renewal{f: f, served: &served{}, clock: &agentClock{}}
	var stop func()
	f, r.a, r.dir, stop = enrolledBy(t, f, func(c *enroll.Config) { c.Now, c.Serve = r.clock.Now, r.served.serve })
	t.Cleanup(stop)
	r.first = r.hostCert()
	return r
}

// hostCert is the certificate beside the host's key.
func (r *renewal) hostCert() *x509.Certificate {
	r.f.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, "host.crt"))
	require.NoError(r.f.t, err)
	block, _ := pem.Decode(b)
	require.NotNil(r.f.t, block)
	c, err := x509.ParseCertificate(block.Bytes)
	require.NoError(r.f.t, err)
	return c
}

func (r *renewal) state() (renewed int, retired []int64, heart int64) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	return len(r.f.renewed), append([]int64{}, r.f.retired...), r.f.heartSerial
}

func TestTheHostRenewsInTheLastNinetyDaysAndRetiresItsOthersWithTheRenewedCertificate(t *testing.T) {
	r := newRenewal(t)
	key, err := os.ReadFile(filepath.Join(r.dir, "host.key"))
	require.NoError(t, err)

	require.NoError(t, r.a.Check(t.Context()))
	r.clock.set(r.first.NotAfter.Add(-identity.HostRenewWindow).Add(-time.Hour))
	require.NoError(t, r.a.Check(t.Context()))
	renewed, _, _ := r.state()
	require.Zero(t, renewed, "more than 90 days left is not due")

	r.clock.set(r.first.NotAfter.Add(-identity.HostRenewWindow).Add(time.Hour))
	r.f.mu.Lock()
	r.f.failRetire = true
	r.f.mu.Unlock()
	require.Error(t, r.a.Check(t.Context()), "the retire failed")
	renewed, retired, _ := r.state()
	require.Equal(t, 1, renewed, "within the window the host renews")
	require.Empty(t, retired)
	got := r.hostCert()
	assert.EqualValues(t, 20, got.SerialNumber.Int64(), "the renewed certificate is written beside the key")
	assert.True(t, got.PublicKey.(*ecdsa.PublicKey).Equal(r.first.PublicKey), "under the host's own key")
	after, err := os.ReadFile(filepath.Join(r.dir, "host.key"))
	require.NoError(t, err)
	assert.Equal(t, key, after, "the key is not rewritten")
	assert.Equal(t, 1, r.served.count(), "and the host API serves it")
	require.Eventually(t, func() bool { _, _, h := r.state(); return h == 20 }, 5*time.Second, 20*time.Millisecond, "the host reports with it")

	r.f.mu.Lock()
	r.f.failRetire = false
	r.f.mu.Unlock()
	require.NoError(t, r.a.Check(t.Context()), "the retire not done is tried again")
	renewed, retired, _ = r.state()
	assert.Equal(t, 1, renewed, "the renewed certificate is not due")
	assert.Equal(t, []int64{20}, retired, "signed by the renewed certificate")

	require.NoError(t, r.a.Check(t.Context()))
	_, retired, _ = r.state()
	assert.Len(t, retired, 1, "a retire done is not tried again")
}

func TestAHostWhoseAPICannotServeTheRenewedCertificateRetiresNothingAndRenewsAgain(t *testing.T) {
	r := newRenewal(t)
	r.clock.set(r.first.NotAfter.Add(-time.Hour))
	r.served.mu.Lock()
	r.served.fail = errors.New("the disk is full")
	r.served.mu.Unlock()
	require.Error(t, r.a.Check(t.Context()))
	_, retired, _ := r.state()
	assert.Empty(t, retired, "a retire would revoke the certificate the API serves")
	assert.EqualValues(t, 10, r.hostCert().SerialNumber.Int64(), "the host keeps the certificate it serves")
	time.Sleep(100 * time.Millisecond)
	_, _, heart := r.state()
	assert.EqualValues(t, 10, heart, "and reports with it")

	r.served.mu.Lock()
	r.served.fail = nil
	r.served.mu.Unlock()
	require.NoError(t, r.a.Check(t.Context()))
	renewed, retired, _ := r.state()
	assert.Equal(t, 2, renewed, "the host renews again")
	assert.Equal(t, []int64{21}, retired, "and retires the others, the first renewal's among them")
}

func TestARenewalTheSignerHoldsNotDueChangesNothing(t *testing.T) {
	r := newRenewal(t)
	r.clock.set(r.first.NotAfter.Add(-time.Hour))
	r.f.mu.Lock()
	r.f.notDue = true
	r.f.mu.Unlock()
	require.NoError(t, r.a.Check(t.Context()), "the signer's clock is behind: the next check renews")
	assert.Equal(t, r.first.Raw, r.hostCert().Raw)
	assert.Zero(t, r.served.count())
}

// authorityCert is a CA certificate of subject cn for key, ending at notAfter.
func authorityCert(t *testing.T, key *ecdsa.PrivateKey, cn string, notAfter time.Time, isCA bool) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(notAfter.UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}

func pemOf(certs ...*x509.Certificate) []byte {
	var b []byte
	for _, c := range certs {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return b
}

func TestTheHostStoresOnlyALaterReSigningOfAnAuthorityItTrustsAndNeverAnotherKeyUnderItsName(t *testing.T) {
	hostKey, operatorKey := mustKey(t), mustKey(t)
	end := time.Now().Add(30 * 24 * time.Hour)
	host := authorityCert(t, hostKey, "naru-host-ca", end, true)
	operator := authorityCert(t, operatorKey, "naru-operator-ca", end, true)
	dir := t.TempDir()
	clientCA, store := filepath.Join(dir, "client-ca.crt"), filepath.Join(dir, "client-ca.pem")
	require.NoError(t, os.WriteFile(clientCA, pemOf(host, operator), 0o600))
	f := newFakeOrch(t, "naru-ek-secret")
	f.life = 365 * 24 * time.Hour // not due to renew
	f, a, _, stop := enrolledBy(t, f, func(c *enroll.Config) { c.ClientCAFile, c.TrustFile = clientCA, store })
	defer stop()

	resigned := authorityCert(t, hostKey, "naru-host-ca", end.AddDate(10, 0, 0), true)
	offer := func(h, o, orch *x509.Certificate) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.offered = identity.Authorities{Host: h.Raw, Operator: o.Raw, Orch: orch.Raw}
	}
	stored := func() []*x509.Certificate {
		b, err := os.ReadFile(store)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		require.NoError(t, err)
		var out []*x509.Certificate
		for {
			var block *pem.Block
			if block, b = pem.Decode(b); block == nil {
				return out
			}
			c, err := x509.ParseCertificate(block.Bytes)
			require.NoError(t, err)
			out = append(out, c)
		}
	}

	offer(
		authorityCert(t, mustKey(t), "naru-host-ca", end.AddDate(10, 0, 0), true),       // a new key under a known name
		authorityCert(t, hostKey, "naru-operator-ca", end.AddDate(10, 0, 0), true),      // a known key under another's name
		authorityCert(t, operatorKey, "naru-operator-ca", end.AddDate(10, 0, 0), false), // not an authority
	)
	require.NoError(t, a.Check(t.Context()))
	assert.Empty(t, stored(), "nothing that is not a re-signing of a trusted authority")

	offer(authorityCert(t, operatorKey, "naru-operator-ca", end.Add(-time.Hour), true), host, host)
	require.NoError(t, a.Check(t.Context()))
	assert.Empty(t, stored(), "nor a re-signing that ends no later, nor the certificate already held")

	offer(resigned, operator, host)
	require.NoError(t, a.Check(t.Context()))
	require.Len(t, stored(), 1)
	assert.Equal(t, resigned.Raw, stored()[0].Raw, "the later re-signing of the host authority")

	require.NoError(t, a.Check(t.Context()))
	assert.Len(t, stored(), 1, "a re-signing stored is not stored again")
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k
}
