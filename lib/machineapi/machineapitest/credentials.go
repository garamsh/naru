package machineapitest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
)

// newKey is a key a caller made where it runs, and its certificate request.
func newKey(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ignored"}}, key)
	require.NoError(t, err)
	return key, csr
}

// Credentials checks a caller's registration and renewal (ADR 0116): a client with no certificate
// reaches register and nothing else; a token registers the caller it names for the caller's own key
// once, and a token spent or never given is refused with one code; Caller renews its pair once, in
// its last 30 days, for a new key and not its current one; a pair not yet due, a pair that renewed
// already, and a role other than caller, renew nothing.
func Credentials(t *testing.T, c Config) {
	c = c.check(t)
	require.NotNil(t, c.Anonymous, "Config.Anonymous")
	require.NotNil(t, c.Token, "Config.Token")
	require.NotNil(t, c.CallerKey, "Config.CallerKey")
	require.NotNil(t, c.CallerCert, "Config.CallerCert")
	ctx := t.Context()

	_, err := c.Anonymous.Machines(ctx)
	refused(t, err, http.StatusForbidden, machineapi.CodeForbidden, "a request with no certificate")

	name := unique("caller")
	token := c.Token(t, name)
	key, csr := newKey(t)
	cred, err := c.Anonymous.Register(ctx, token, csr)
	require.NoError(t, err, "a token given for the caller registers it")
	cert, err := x509.ParseCertificate(cred.Certificate)
	require.NoError(t, err)
	assert.Equal(t, name, cert.Subject.CommonName, "the token names the caller")
	assert.Equal(t, []string{"caller"}, cert.Subject.OrganizationalUnit)
	assert.True(t, cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey), "the certificate is for the key the caller made")
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, cert.ExtKeyUsage, "client authentication only")
	assert.Equal(t, cert.NotAfter, cred.NotAfter)

	_, again := newKey(t)
	_, err = c.Anonymous.Register(ctx, token, again)
	refused(t, err, http.StatusForbidden, machineapi.CodeTokenRefused, "a token spent twice")
	_, err = c.Anonymous.Register(ctx, "naru-ct-"+unique("made-up"), again)
	refused(t, err, http.StatusForbidden, machineapi.CodeTokenRefused, "a token never given")

	sameCSR, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ignored"}}, c.CallerKey)
	require.NoError(t, err)
	_, err = c.Caller.Renew(ctx, c.CallerKey, c.CallerCert, sameCSR)
	refused(t, err, http.StatusBadRequest, machineapi.CodeInvalid, "a renewal for the current key")

	nextKey, nextCSR := newKey(t)
	renewed, err := c.Caller.Renew(ctx, c.CallerKey, c.CallerCert, nextCSR)
	require.NoError(t, err, "Caller's pair is in its last 30 days")
	next, err := x509.ParseCertificate(renewed.Certificate)
	require.NoError(t, err)
	assert.Equal(t, c.CallerCert.Subject.CommonName, next.Subject.CommonName, "the name is the current certificate's")
	assert.True(t, next.PublicKey.(*ecdsa.PublicKey).Equal(&nextKey.PublicKey), "the certificate is for the new key")
	_, err = c.Caller.Renew(ctx, c.CallerKey, c.CallerCert, again)
	refused(t, err, http.StatusForbidden, machineapi.CodeForbidden, "a pair that renewed already")

	_, err = c.Caller.Renew(ctx, key, cert, again)
	refused(t, err, http.StatusConflict, machineapi.CodeNotDue, "a pair of 90 days just registered")
	if c.OtherRole != nil {
		_, err = c.OtherRole.Renew(ctx, c.CallerKey, c.CallerCert, again)
		refused(t, err, http.StatusForbidden, machineapi.CodeForbidden, "a certificate of another role")
	}
}
