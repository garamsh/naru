package orchd_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// keyAndCSR is a key a caller made and its certificate request.
func keyAndCSR(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ignored"}}, key)
	require.NoError(t, err)
	return key, csr
}

// held is the identity of a key and the credential the API answered for it.
func held(t *testing.T, key *ecdsa.PrivateKey, cred machineapi.Credential) orchtest.Identity {
	t.Helper()
	cert, err := x509.ParseCertificate(cred.Certificate)
	require.NoError(t, err)
	return orchtest.Identity{TLS: tls.Certificate{Certificate: [][]byte{cred.Certificate}, PrivateKey: key, Leaf: cert}, Cert: cert}
}

func TestOrch_ACallerRegistersWithoutACertificateAndUsesTheOneItGets(t *testing.T) {
	o := startMachineOrch(t)
	anon := o.c.MachineClient(t, 0, orchtest.Identity{})
	_, err := anon.Machines(t.Context())
	refused(t, err, http.StatusForbidden, machineapi.CodeForbidden, "no certificate reaches no other operation")

	token := o.c.Signer.CallerToken("ws-provider")
	key, csr := keyAndCSR(t)
	cred, err := anon.Register(t.Context(), token, csr)
	require.NoError(t, err)
	id := held(t, key, cred)
	assert.Equal(t, "ws-provider", id.Cert.Subject.CommonName, "the token names the caller")
	assert.Equal(t, []string{string(identity.RoleCaller)}, id.Cert.Subject.OrganizationalUnit)
	assert.True(t, id.Cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey), "for the key the caller made")

	_, created, err := o.c.MachineClient(t, 0, id).CreateMachine(t.Context(), "web", machineSpec())
	require.NoError(t, err, "the registered caller calls the API")
	assert.True(t, created)

	_, csr = keyAndCSR(t)
	_, err = anon.Register(t.Context(), token, csr)
	refused(t, err, http.StatusForbidden, machineapi.CodeTokenRefused, "a token spent twice")
	_, err = anon.Register(t.Context(), "naru-ct-made-up", csr)
	refused(t, err, http.StatusForbidden, machineapi.CodeTokenRefused, "a token never given")
	_, err = anon.Register(t.Context(), o.c.Signer.CallerToken("ws-provider"), []byte("not a request"))
	refused(t, err, http.StatusBadRequest, machineapi.CodeInvalid, "a request that does not parse")
}

func TestOrch_ACallerRenewsInItsLastThirtyDaysForANewKey(t *testing.T) {
	o := startMachineOrch(t)
	key, csr := keyAndCSR(t)
	cred, err := o.garam.Renew(t.Context(), o.garamID.TLS.PrivateKey.(*ecdsa.PrivateKey), o.garamID.Cert, csr)
	require.NoError(t, err, "garam's certificate lasts 12 hours, so it is due")
	next := held(t, key, cred)
	assert.Equal(t, "garam", next.Cert.Subject.CommonName, "the name is the current certificate's")
	assert.True(t, next.Cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey))

	_, csr = keyAndCSR(t)
	_, err = o.c.MachineClient(t, 0, next).Renew(t.Context(), key, next.Cert, csr)
	refused(t, err, http.StatusConflict, machineapi.CodeNotDue, "the new 90-day certificate is not due")

	op := o.c.CA.Issue(t, "alice", identity.RoleOperator)
	_, err = o.op.Renew(t.Context(), op.TLS.PrivateKey.(*ecdsa.PrivateKey), op.Cert, csr)
	refused(t, err, http.StatusForbidden, machineapi.CodeForbidden, "an operator renews nothing")
	_, err = o.garam.Renew(t.Context(), op.TLS.PrivateKey.(*ecdsa.PrivateKey), op.Cert, csr)
	refused(t, err, http.StatusForbidden, machineapi.CodeForbidden, "nor does a caller with an operator's signed request")
}

func TestOrch_RegistrationsFromOneAddressAreBounded(t *testing.T) {
	o := startMachineOrch(t)
	anon := o.c.MachineClient(t, 0, orchtest.Identity{})
	_, csr := keyAndCSR(t)
	for range 10 {
		_, err := anon.Register(t.Context(), "naru-ct-made-up", csr)
		refused(t, err, http.StatusForbidden, machineapi.CodeTokenRefused, "the control: within the burst the token is what refuses")
	}
	token := o.c.Signer.CallerToken("ws-provider")
	_, err := anon.Register(t.Context(), token, csr)
	refused(t, err, http.StatusTooManyRequests, machineapi.CodeRateLimited, "the eleventh, with a good token, waits")

	o.c.Clock.Advance(time.Second)
	_, err = anon.Register(t.Context(), token, csr)
	require.NoError(t, err, "a second later the same token registers: the limit spent nothing of it")
}
