package signer_test

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

// newOrchRenewalEnv is newRenewalEnv for the orch member orch-1, issued by root under the DNS
// name orch-1.example and the IP address 10.0.0.9. Without the registry, the signer is reopened
// with no orchs.json, as a directory made before it is.
func newOrchRenewalEnv(t *testing.T, registry bool) *renewalEnv {
	t.Helper()
	e := newEnv(t)
	key := newKey(t)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "orch-1"}, DNSNames: []string{"orch-1.example"}, IPAddresses: []net.IP{net.ParseIP("10.0.0.9")}}, key)
	require.NoError(t, err)
	status, body := e.do(http.MethodPost, "/v1/orch", e.signed(e.rootID, identity.ActionIssueOrch, identity.IssueRequest{Name: "orch-1", CSR: csr}))
	require.Equal(t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	r := &renewalEnv{env: e, clock: &clock{}, key: key, host: person{key: key, cert: parseCert(t, issued.Certificate)}, operator: e.issue(identity.RoleOperator, "bob")}
	require.Equal(t, []string{"orch-1.example"}, r.host.cert.DNSNames, "setup: the certificate names its DNS name")

	e.srv.Close()
	if !registry {
		require.NoError(t, os.Remove(filepath.Join(e.dir, "orchs.json")))
	}
	s, err := signer.Open(signer.Options{Dir: e.dir, Log: quiet, Now: r.clock.now})
	require.NoError(t, err)
	e.s, e.srv = s, httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)
	return r
}

func (r *renewalEnv) renewOrch(p person, key *ecdsa.PrivateKey) (int, []byte) {
	return r.do(http.MethodPost, "/v1/orch-renew", r.signedAt(p, identity.ActionRenewOrch, identity.OrchRenewal{CSR: csrFor(r.t, key)}))
}

func (r *renewalEnv) renewedOrch(p person) person {
	r.t.Helper()
	status, body := r.renewOrch(p, p.key)
	require.Equal(r.t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(r.t, json.Unmarshal(body, &issued))
	return person{key: p.key, cert: parseCert(r.t, issued.Certificate)}
}

func (r *renewalEnv) retireOrch(p person) int {
	status, _ := r.do(http.MethodPost, "/v1/orch-retire", r.signedAt(p, identity.ActionRetireOwnOrch, struct{}{}))
	return status
}

func TestAnOrchMemberRenewsUnderItsOwnKeyInTheLastNinetyDaysOfItsCertificate(t *testing.T) {
	r := newOrchRenewalEnv(t, true)
	old := r.host.cert

	status, body := r.renewOrch(r.host, r.key)
	assert.Equal(t, http.StatusConflict, status, "a year left is not due: %s", body)

	r.at(old.NotAfter.Add(-identity.OrchRenewWindow).Add(time.Hour))
	status, body = r.renewOrch(r.host, newKey(t))
	assert.Equal(t, http.StatusBadRequest, status, "a renewal for another key: %s", body)
	status, _ = r.do(http.MethodPost, "/v1/orch-renew", r.signedAt(r.operator, identity.ActionRenewOrch, identity.OrchRenewal{CSR: csrFor(t, r.operator.key)}))
	assert.Equal(t, http.StatusForbidden, status, "an operator's certificate does not chain to the orch authority")

	got := r.renewedOrch(r.host).cert
	assert.Equal(t, "orch-1", got.Subject.CommonName, "control: due, signed by the member, for its key")
	assert.Equal(t, old.DNSNames, got.DNSNames, "the current certificate's DNS names")
	assert.Equal(t, old.IPAddresses, got.IPAddresses, "and its addresses")
	assert.True(t, got.PublicKey.(*ecdsa.PublicKey).Equal(&r.key.PublicKey), "the member's own key")
	assert.Equal(t, identity.RoleOrch, roleOf(t, got))
	assert.True(t, got.NotAfter.After(old.NotAfter))
	_, err := got.Verify(x509.VerifyOptions{Roots: poolOf(t, r.authorities().Orch), CurrentTime: r.clock.now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}})
	require.NoError(t, err, "under the orch authority")
	assert.False(t, r.revoked(identity.SerialOf(old)), "the old certificate stays valid until the member retires it")
}

func TestARenewedOrchMemberRetiresItsOthersWithTheCertificateItKeeps(t *testing.T) {
	for _, registry := range []bool{true, false} {
		r := newOrchRenewalEnv(t, registry)
		old := r.host.cert
		r.at(old.NotAfter.Add(-identity.OrchRenewWindow).Add(time.Hour))
		lost, kept := r.renewedOrch(r.host), r.renewedOrch(r.host)
		assert.True(t, r.revoked(identity.SerialOf(lost.cert)), "registry %v: one unretired renewal: the second revokes the first", registry)
		assert.False(t, r.revoked(identity.SerialOf(old)))

		require.Equal(t, http.StatusOK, r.retireOrch(kept))
		assert.True(t, r.revoked(identity.SerialOf(old)), "registry %v: the certificate it renewed from, recorded at the renewal when the registry had none", registry)
		assert.False(t, r.revoked(identity.SerialOf(kept.cert)), "control: not the one that signed the retire")
		require.Equal(t, http.StatusOK, r.retireOrch(kept), "a retry changes nothing")
		assert.Equal(t, http.StatusForbidden, r.retireOrch(lost), "a revoked certificate retires nothing")
		assert.Equal(t, http.StatusForbidden, r.retireOrch(r.operator), "nor does an operator's")
		status, _ := r.renewOrch(person{key: r.key, cert: old}, r.key)
		assert.Equal(t, http.StatusForbidden, status, "a retired certificate renews nothing")
	}
}

func TestAnOrchRenewalRacingARevocationIssuesNothing(t *testing.T) {
	r := newOrchRenewalEnv(t, true)
	r.at(r.host.cert.NotAfter.Add(-identity.OrchRenewWindow).Add(time.Hour))
	revoked := false
	defer signer.SetRenewHook(func() {
		if revoked {
			return
		}
		revoked = true
		status, body := r.do(http.MethodPost, "/v1/revoke", r.signedAt(r.operator, identity.ActionRevoke, identity.RevokeRequest{Serial: identity.SerialOf(r.host.cert)}))
		require.Equal(t, http.StatusOK, status, string(body))
	})()
	status, body := r.renewOrch(r.host, r.key)
	assert.Equal(t, http.StatusForbidden, status, "the revocation that landed first ends the renewal: %s", body)
	require.True(t, revoked, "setup: the revocation ran between the verification and the issue")
}
