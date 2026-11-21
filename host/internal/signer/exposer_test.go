package signer_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// exposer asks /v1/exposer, as by, for network and days, with a fresh key.
func (e *env) exposer(by person, network string, days int) (int, []byte) {
	return e.do(http.MethodPost, "/v1/exposer", e.signed(by, identity.ActionIssueExposer,
		identity.ExposerRequest{Network: network, Days: days, CSR: csrFor(e.t, newKey(e.t))}))
}

func (e *env) issuedExposer(by person, network string, days int) person {
	e.t.Helper()
	key := newKey(e.t)
	status, body := e.do(http.MethodPost, "/v1/exposer", e.signed(by, identity.ActionIssueExposer,
		identity.ExposerRequest{Network: network, Days: days, CSR: csrFor(e.t, key)}))
	require.Equal(e.t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(e.t, json.Unmarshal(body, &issued))
	return person{key: key, cert: parseCert(e.t, issued.Certificate)}
}

func TestExposer_RootAndAnOperatorGetACertificateOfTheNetworkAndNoOtherWayToAskForAName(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	for _, by := range []person{e.rootID, op} {
		p := e.issuedExposer(by, "prod", 0)
		assert.Equal(t, "exposer.prod", p.cert.Subject.CommonName, "the CN is made from the network, not the request's own (\"ignored\")")
		assert.Equal(t, identity.RoleExposer, roleOf(t, p.cert))
		assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, p.cert.ExtKeyUsage)
		_, err := p.cert.Verify(x509.VerifyOptions{Roots: poolOf(t, e.authorities().Operator), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		assert.NoError(t, err, "it chains to the operator authority, which orch trusts")
		id, err := identity.Authenticate([][]*x509.Certificate{{p.cert}}, &identity.CRL{})
		require.NoError(t, err)
		n, ok := identity.ExposerNetwork(id)
		assert.True(t, ok)
		assert.Equal(t, "prod", n, "orch reads the network from the certificate")
	}
}

func TestExposer_OnlyRootAndOperatorsMayAskAndTheSignatureIsForThisAction(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	reader := e.issue(identity.RoleReader, "bob")
	auto := e.issue(identity.RoleAutomation, "ci")
	exposer := e.issuedExposer(op, "prod", 0)

	for name, by := range map[string]person{"a reader": reader, "automation": auto, "an exposer": exposer} {
		status, body := e.exposer(by, "prod", 0)
		assert.Equal(t, http.StatusForbidden, status, "%s: %s", name, body)
	}
	status, _ := e.do(http.MethodPost, "/v1/exposer", e.signed(op, identity.ActionIssueOperator,
		identity.ExposerRequest{Network: "prod", CSR: csrFor(t, newKey(t))}))
	assert.Equal(t, http.StatusForbidden, status, "a request signed for another action is refused")
	status, body := e.exposer(op, "prod", 0)
	assert.Equal(t, http.StatusOK, status, "the control: an operator is accepted: %s", body)
}

func TestExposer_RefusesANetworkThatIsNotADNSLabel(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"", "Prod", "a.b", "-x", "x_", "exposer.prod", "a b", "x/y", "ünï"} {
		status, body := e.exposer(e.rootID, bad, 0)
		assert.Equal(t, http.StatusBadRequest, status, "network %q: %s", bad, body)
	}
	status, body := e.exposer(e.rootID, "prod-1", 0)
	assert.Equal(t, http.StatusOK, status, "the control: a label is accepted: %s", body)
}

func TestExposer_IsValidAtMostNinetyDays(t *testing.T) {
	e := newEnv(t)
	for _, days := range []int{91, 365, -1} {
		status, body := e.exposer(e.rootID, "prod", days)
		assert.Equal(t, http.StatusBadRequest, status, "%d days: %s", days, body)
	}
	for days, want := range map[int]int{0: 90, 90: 90, 30: 30, 1: 1} {
		p := e.issuedExposer(e.rootID, "prod", days)
		assert.WithinDuration(t, time.Now().AddDate(0, 0, want), p.cert.NotAfter, time.Minute, "%d days asked", days)
	}
}

func TestExposer_ItsSerialIsRevocable(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	p := e.issuedExposer(op, "prod", 0)
	status, body := e.revoke(op, p.cert.SerialNumber.Text(16))
	require.Equal(t, http.StatusOK, status, string(body))
	crlBytes, err := e.s.CRL()
	require.NoError(t, err)
	crl, err := identity.VerifyCRL(ed25519.PublicKey(e.authorities().RevocationKey), crlBytes)
	require.NoError(t, err)
	_, err = identity.Authenticate([][]*x509.Certificate{{p.cert}}, crl)
	assert.ErrorIs(t, err, identity.ErrRevoked)
}
