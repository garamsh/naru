package signer_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// caller asks /v1/caller, as by, for name and days, with a fresh key.
func (e *env) caller(by person, name string, days int) (int, []byte) {
	return e.do(http.MethodPost, "/v1/caller-issue", e.signed(by, identity.ActionIssueCaller,
		identity.CallerRequest{Name: name, Days: days, CSR: csrFor(e.t, newKey(e.t))}))
}

func (e *env) issuedCaller(by person, name string, days int, csr []byte) person {
	e.t.Helper()
	key := newKey(e.t)
	if csr == nil {
		csr = csrFor(e.t, key)
	}
	status, body := e.do(http.MethodPost, "/v1/caller-issue", e.signed(by, identity.ActionIssueCaller,
		identity.CallerRequest{Name: name, Days: days, CSR: csr}))
	require.Equal(e.t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(e.t, json.Unmarshal(body, &issued))
	return person{key: key, cert: parseCert(e.t, issued.Certificate)}
}

func TestCaller_RootGetsAClientCertificateNamedByTheBodyAndNotByTheRequest(t *testing.T) {
	e := newEnv(t)
	key := newKey(t)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: "root", OrganizationalUnit: []string{"root"}, Organization: []string{"naru:host-provisioner"}},
		DNSNames: []string{"orch.example"},
	}, key)
	require.NoError(t, err)
	p := e.issuedCaller(e.rootID, "garam", 0, der)

	assert.Equal(t, "garam", p.cert.Subject.CommonName, "the CN is the body's name, not the request's (\"root\")")
	assert.Equal(t, identity.RoleCaller, roleOf(t, p.cert), "the role is caller, not the request's OU")
	assert.Empty(t, p.cert.DNSNames, "no name the request asks for is carried")
	assert.Empty(t, p.cert.Subject.Organization, "no Organization is carried, which the host API would read as a role")
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, p.cert.ExtKeyUsage, "client authentication only")
	_, err = p.cert.Verify(x509.VerifyOptions{Roots: poolOf(t, e.authorities().Operator), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "it chains to the operator authority, which orch trusts")
	_, err = p.cert.Verify(x509.VerifyOptions{Roots: poolOf(t, e.authorities().Operator), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	assert.Error(t, err, "it cannot serve")
	id, err := identity.Authenticate([][]*x509.Certificate{{p.cert}}, &identity.CRL{})
	require.NoError(t, err)
	name, ok := identity.CallerName(id)
	assert.True(t, ok)
	assert.Equal(t, "garam", name, "orch reads the caller's name from the certificate")
}

func TestCaller_OnlyRootMayAskAndTheSignatureIsForThisAction(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	reader := e.issue(identity.RoleReader, "bob")
	auto := e.issue(identity.RoleAutomation, "ci")
	caller := e.issuedCaller(e.rootID, "garam", 0, nil)

	for name, by := range map[string]person{"an operator": op, "a reader": reader, "automation": auto, "a caller": caller} {
		status, body := e.caller(by, "garam", 0)
		assert.Equal(t, http.StatusForbidden, status, "%s: %s", name, body)
	}
	status, _ := e.do(http.MethodPost, "/v1/caller-issue", e.signed(e.rootID, identity.ActionIssueOperator,
		identity.CallerRequest{Name: "garam", CSR: csrFor(t, newKey(t))}))
	assert.Equal(t, http.StatusForbidden, status, "a request root signed for another action is refused")
	status, body := e.caller(e.rootID, "garam", 0)
	assert.Equal(t, http.StatusOK, status, "the control: root is accepted: %s", body)
}

func TestCaller_RefusesANameThatIsNotADNSLabel(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{"", "Garam", "a.b", "-x", "x_", "a b", "x/y", "ünï", "root@x"} {
		status, body := e.caller(e.rootID, bad, 0)
		assert.Equal(t, http.StatusBadRequest, status, "name %q: %s", bad, body)
	}
	status, body := e.caller(e.rootID, "use-1", 0)
	assert.Equal(t, http.StatusOK, status, "the control: a label is accepted: %s", body)
}

func TestCaller_IsValidNinetyDaysByDefaultAndAtMostNinety(t *testing.T) {
	e := newEnv(t)
	for _, days := range []int{91, 365, -1} {
		status, body := e.caller(e.rootID, "garam", days)
		assert.Equal(t, http.StatusBadRequest, status, "%d days: %s", days, body)
	}
	for days, want := range map[int]int{0: 90, 90: 90, 30: 30, 1: 1} {
		p := e.issuedCaller(e.rootID, "garam", days, nil)
		assert.WithinDuration(t, time.Now().AddDate(0, 0, want), p.cert.NotAfter, time.Minute, "%d days asked", days)
	}
}

func TestCaller_ItsSerialIsRecordedAsCallerAndRevocable(t *testing.T) {
	e := newEnv(t)
	p := e.issuedCaller(e.rootID, "garam", 0, nil)
	serial := p.cert.SerialNumber.Text(16)
	data, err := os.ReadFile(filepath.Join(e.dir, "issued.json"))
	require.NoError(t, err)
	var roles map[string]string
	require.NoError(t, json.Unmarshal(data, &roles))
	assert.Equal(t, string(identity.RoleCaller), roles[serial], "the serial is recorded as role caller")

	status, body := e.revoke(e.rootID, serial)
	require.Equal(t, http.StatusOK, status, string(body))
	crlBytes, err := e.s.CRL()
	require.NoError(t, err)
	crl, err := identity.VerifyCRL(ed25519.PublicKey(e.authorities().RevocationKey), crlBytes)
	require.NoError(t, err)
	_, err = identity.Authenticate([][]*x509.Certificate{{p.cert}}, crl)
	assert.ErrorIs(t, err, identity.ErrRevoked)
}
