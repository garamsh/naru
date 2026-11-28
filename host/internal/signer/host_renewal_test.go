package signer_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

// renewalEnv is newEnv's signer opened again on its directory with a clock the test moves, and the
// host certificate issued to host-1 for key, with the IP address 10.0.0.5.
type renewalEnv struct {
	*env
	clock *clock
	key   *ecdsa.PrivateKey
	host  person
	// operator is an operator issued before the clock moved.
	operator person
}

func newRenewalEnv(t *testing.T) *renewalEnv {
	t.Helper()
	e := newEnv(t)
	key := newKey(t)
	op := e.issue(identity.RoleOperator, "alice")
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "host-1"}, IPAddresses: []net.IP{net.ParseIP("10.0.0.5")}}, key)
	require.NoError(t, err)
	stmt, err := identity.SignAdmission(identity.Admission{HostID: "host-1", HostKeySHA256: keyHash(t, csr), Region: "kr", ExpiresAt: time.Now().Add(time.Hour)}, op.key, op.cert)
	require.NoError(t, err)
	b, err := json.Marshal(identity.HostRequest{Statement: stmt, CSR: csr})
	require.NoError(t, err)
	status, body := e.do(http.MethodPost, "/v1/host", b)
	require.Equal(t, http.StatusOK, status, string(body))
	// An operator's rotation gives the host a certificate that names its address, as ADR 0077 does.
	status, body = e.do(http.MethodPost, "/v1/reissue-host", e.signed(op, identity.ActionReissueHost, identity.ReissueHostRequest{HostID: "host-1", CSR: csr}))
	require.Equal(t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	r := &renewalEnv{env: e, clock: &clock{}, key: key, host: person{key: key, cert: parseCert(t, issued.Certificate)}, operator: e.issue(identity.RoleOperator, "bob")}
	require.Equal(t, []net.IP{net.ParseIP("10.0.0.5").To4()}, r.host.cert.IPAddresses, "setup: the host's certificate names its address")

	e.srv.Close()
	s, err := signer.Open(signer.Options{Dir: e.dir, Log: quiet, Now: r.clock.now})
	require.NoError(t, err)
	e.s, e.srv = s, httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)
	return r
}

// at moves the env's clock to t.
func (r *renewalEnv) at(t time.Time) { r.clock.advance(t.Sub(r.clock.now())) }

// signedAt is a request by p, expiring a minute after the env's clock.
func (r *renewalEnv) signedAt(p person, action string, body any) []byte {
	r.t.Helper()
	b, err := identity.SignRequest(action, body, r.clock.now().Add(time.Minute), p.key, p.cert)
	require.NoError(r.t, err)
	return b
}

func (r *renewalEnv) renew(p person, key *ecdsa.PrivateKey) (int, []byte) {
	return r.do(http.MethodPost, "/v1/host-renew", r.signedAt(p, identity.ActionRenewHost, identity.HostRenewal{CSR: csrFor(r.t, key)}))
}

// revoked reports whether the signer's list revokes serial.
func (r *renewalEnv) revoked(serial string) bool {
	r.t.Helper()
	status, body := r.do(http.MethodGet, "/v1/crl", nil)
	require.Equal(r.t, http.StatusOK, status)
	list, err := identity.VerifyCRL(ed25519.PublicKey(r.authorities().RevocationKey), body)
	require.NoError(r.t, err)
	return list.Revokes(serial)
}

func TestAHostRenewsUnderItsOwnKeyInTheLastNinetyDaysOfItsCertificate(t *testing.T) {
	r := newRenewalEnv(t)
	old := r.host.cert

	status, body := r.renew(r.host, r.key)
	assert.Equal(t, http.StatusConflict, status, "a year left is not due: %s", body)

	r.at(old.NotAfter.Add(-identity.HostRenewWindow).Add(time.Hour))
	status, body = r.renew(r.host, newKey(t))
	assert.Equal(t, http.StatusBadRequest, status, "a renewal for another key: %s", body)
	status, _ = r.do(http.MethodPost, "/v1/host-renew", r.signedAt(r.operator, identity.ActionRenewHost, identity.HostRenewal{CSR: csrFor(t, r.operator.key)}))
	assert.Equal(t, http.StatusForbidden, status, "an operator's certificate does not chain to the host authority")

	status, body = r.renew(r.host, r.key)
	require.Equal(t, http.StatusOK, status, "control: due, signed by the host, for its key: %s", body)
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	got := parseCert(t, issued.Certificate)
	assert.Equal(t, "host-1", got.Subject.CommonName)
	assert.Equal(t, []string{"host-1"}, got.DNSNames)
	assert.Equal(t, old.IPAddresses, got.IPAddresses, "the current certificate's addresses")
	assert.True(t, got.PublicKey.(*ecdsa.PublicKey).Equal(&r.key.PublicKey), "the host's own key")
	assert.Equal(t, identity.RoleHost, roleOf(t, got))
	assert.True(t, got.NotAfter.After(old.NotAfter), "a year from the renewal")
	_, err := got.Verify(x509.VerifyOptions{Roots: poolOf(t, r.authorities().Host), CurrentTime: r.clock.now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err, "under the host authority")
	assert.False(t, r.revoked(identity.SerialOf(old)), "the old certificate stays valid until the host retires it")
}

func TestARenewedHostRetiresEveryOtherCertificateByTheOneItKeeps(t *testing.T) {
	r := newRenewalEnv(t)
	old := r.host.cert
	r.at(old.NotAfter.Add(-identity.HostRenewWindow).Add(time.Hour))
	renew := func() person {
		status, body := r.renew(r.host, r.key)
		require.Equal(t, http.StatusOK, status, string(body))
		var issued identity.Issued
		require.NoError(t, json.Unmarshal(body, &issued))
		return person{key: r.key, cert: parseCert(t, issued.Certificate)}
	}
	lost, kept := renew(), renew() // the first answer was lost, so the host renewed again

	retire := func(p person) int {
		status, _ := r.do(http.MethodPost, "/v1/host-retire", r.signedAt(p, identity.ActionRetireOwnHost, struct{}{}))
		return status
	}
	require.Equal(t, http.StatusOK, retire(kept))
	assert.True(t, r.revoked(identity.SerialOf(old)), "the certificate it renewed from")
	assert.True(t, r.revoked(identity.SerialOf(lost.cert)), "and the one whose answer was lost")
	assert.False(t, r.revoked(identity.SerialOf(kept.cert)), "control: not the one that signed the retire")
	require.Equal(t, http.StatusOK, retire(kept), "a retry changes nothing")
	assert.False(t, r.revoked(identity.SerialOf(kept.cert)))
	assert.Equal(t, http.StatusForbidden, retire(lost), "a revoked certificate retires nothing")
	assert.Equal(t, http.StatusForbidden, retire(r.operator), "nor does an operator's")

	status, _ := r.renew(person{key: r.key, cert: old}, r.key)
	assert.Equal(t, http.StatusForbidden, status, "a retired certificate renews nothing")
}

func TestARenewalRacingARevocationOfItsHostIssuesNothing(t *testing.T) {
	r := newRenewalEnv(t)
	r.at(r.host.cert.NotAfter.Add(-identity.HostRenewWindow).Add(time.Hour))
	before := r.serials()
	revoked := false
	restore := signer.SetRenewHook(func() {
		if revoked {
			return
		}
		revoked = true
		// An operator revokes the host after its renewal was verified and before it is issued.
		status, body := r.do(http.MethodPost, "/v1/revoke-host", r.signedAt(r.operator, identity.ActionRevokeHost, identity.HostSerialsRequest{HostID: "host-1"}))
		require.Equal(t, http.StatusOK, status, string(body))
	})
	defer restore()
	status, body := r.renew(r.host, r.key)
	assert.Equal(t, http.StatusForbidden, status, "the revocation that landed first ends the renewal: %s", body)
	require.True(t, revoked, "setup: the revocation ran between the verification and the issue")
	held := r.serials()
	assert.Equal(t, before, held, "no certificate was issued past the revocation")
	for _, serial := range held {
		assert.True(t, r.revoked(serial), "every serial the host holds is revoked")
	}
}

func TestAHostRenewsWhenNothingRevokesItWhileTheHookRuns(t *testing.T) {
	r := newRenewalEnv(t)
	r.at(r.host.cert.NotAfter.Add(-identity.HostRenewWindow).Add(time.Hour))
	ran := false
	defer signer.SetRenewHook(func() { ran = true })()
	status, body := r.renew(r.host, r.key)
	assert.Equal(t, http.StatusOK, status, "control: the same renewal, with no revocation in between: %s", body)
	assert.True(t, ran)
}

// serials are the serials the signer holds for host-1.
func (r *renewalEnv) serials() []string {
	r.t.Helper()
	status, body := r.do(http.MethodPost, "/v1/lookup-host", r.signedAt(r.operator, identity.ActionLookupHost, identity.HostSerialsRequest{HostID: "host-1"}))
	require.Equal(r.t, http.StatusOK, status, string(body))
	var got identity.HostSerials
	require.NoError(r.t, json.Unmarshal(body, &got))
	var out []string
	for _, h := range got.Serials {
		out = append(out, h.Serial)
	}
	return out
}
