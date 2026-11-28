package orchd_test

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/orchtest"
	"github.com/garamsh/naru/orch/internal/signer"
)

// signedBy is a request id signs for action with the body.
func signedBy(t *testing.T, id orchtest.Identity, action string, body any) []byte {
	t.Helper()
	b, err := identity.SignRequest(action, body, time.Now().Add(time.Minute), id.TLS.PrivateKey.(crypto.Signer), id.Cert)
	require.NoError(t, err)
	return b
}

// renewal is id's renewal under its own key.
func renewal(t *testing.T, id orchtest.Identity) []byte {
	t.Helper()
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id.Cert.Subject.CommonName}}, id.TLS.PrivateKey.(crypto.Signer))
	require.NoError(t, err)
	return signedBy(t, id, identity.ActionRenewHost, identity.HostRenewal{CSR: csr})
}

func TestOrchPassesOnAHostsRenewalAndRetireOnlyWhenTheCertificateItConnectsWithSignedThem(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	a, b := o.c.CA.Issue(t, "host-a", identity.RoleHost), o.c.CA.Issue(t, "host-b", identity.RoleHost)
	aAgain := o.c.CA.Issue(t, "host-a", identity.RoleHost) // another certificate of host-a's name
	hostA := o.c.Client(t, 0, &a)

	issued, err := hostA.RenewHost(t.Context(), renewal(t, a))
	require.NoError(t, err, "control: host-a's own renewal")
	cert, err := x509.ParseCertificate(issued.Certificate)
	require.NoError(t, err)
	assert.Equal(t, "host-a", cert.Subject.CommonName)
	assert.True(t, cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool }).Equal(a.Cert.PublicKey), "under host-a's key")

	// The signer would issue each of these renewals, so orch must refuse them itself.
	for name, signed := range map[string][]byte{
		"another host's renewal":               renewal(t, b),
		"a renewal another certificate signed": renewal(t, aAgain),
		"its retire, passed on as a renewal":   signedBy(t, a, identity.ActionRetireOwnHost, struct{}{}),
	} {
		_, err := hostA.RenewHost(t.Context(), signed)
		require.Equal(t, http.StatusForbidden, status(err), "%s: %v", name, err)
		assert.Contains(t, body(t, err), "a host passes on only a request of its own", "%s: orch refuses it, not the signer", name)
	}
	_, err = hostA.RenewHost(t.Context(), []byte(`{"action":"renew-host"}`))
	assert.Equal(t, http.StatusBadRequest, status(err), "a body that is not a signed request")

	o.c.Signer.FailRenew(fmt.Errorf("%w: from then", identity.ErrHostNotDue))
	_, err = hostA.RenewHost(t.Context(), renewal(t, a))
	assert.Equal(t, http.StatusConflict, status(err), "a renewal before its window: %v", err)
	assert.Equal(t, identity.ErrHostNotDue.Error(), body(t, err))
	o.c.Signer.FailRenew(nil)

	require.NoError(t, hostA.RetireHost(t.Context(), signedBy(t, a, identity.ActionRetireOwnHost, struct{}{})), "control: host-a's own retire")
	assert.Equal(t, http.StatusForbidden, status(hostA.RetireHost(t.Context(), signedBy(t, b, identity.ActionRetireOwnHost, struct{}{}))), "host-b's retire")
	assert.Equal(t, http.StatusForbidden, status(hostA.RetireHost(t.Context(), signedBy(t, aAgain, identity.ActionRetireOwnHost, struct{}{}))), "a retire another certificate of host-a signed")
	assert.Equal(t, http.StatusForbidden, status(hostA.RetireHost(t.Context(), renewal(t, a))), "a renewal passed on as a retire")
	assert.Equal(t, []string{identity.SerialOf(a.Cert)}, o.c.Signer.Retired(), "only host-a's own retire reached the signer")
	o.c.Signer.FailRetire(fmt.Errorf("%w: serial 0a", signer.ErrUnknownHost))
	err = hostA.RetireHost(t.Context(), signedBy(t, a, identity.ActionRetireOwnHost, struct{}{}))
	assert.Equal(t, http.StatusConflict, status(err), "a serial the signer holds no record of: %v", err)
	o.c.Signer.FailRetire(nil)

	_, err = o.op.RenewHost(t.Context(), renewal(t, o.operator))
	assert.Equal(t, http.StatusForbidden, status(err), "an operator renews no host")
}

func TestOrchAnswersTheSignersAuthoritiesToAHostAlone(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	a := o.c.CA.Issue(t, "host-a", identity.RoleHost)
	got, err := o.c.Client(t, 0, &a).Authorities(t.Context())
	require.NoError(t, err)
	assert.True(t, o.c.CA.Pool().Equal(poolOf(t, got.Host)), "the signer's host authority")

	_, err = o.reader.Authorities(t.Context())
	assert.Equal(t, http.StatusForbidden, status(err), "a reader")
	_, err = o.anon.Authorities(t.Context())
	assert.Equal(t, http.StatusUnauthorized, status(err), "a caller with no certificate")
}

func poolOf(t *testing.T, der []byte) *x509.CertPool {
	t.Helper()
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	p := x509.NewCertPool()
	p.AddCert(cert)
	return p
}
