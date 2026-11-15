package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/init/api"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/pkitest"
)

// revokingRig is a Node API that runs a signer and checks its clients against the list the signer
// serves. serve changes the answer of the signer to the list given, signed by the key returned.
type revokingRig struct {
	*rig
	list atomic.Pointer[[]byte]
	key  ed25519.PrivateKey
}

func (v *revokingRig) revoke(t *testing.T, seq uint64, serials ...string) {
	t.Helper()
	b, err := identity.SignCRL(v.key, identity.CRL{Seq: seq, IssuedAt: time.Now(), Serials: serials})
	require.NoError(t, err)
	v.list.Store(&b)
}

func newRevokingRig(t *testing.T) *revokingRig {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubPEM, err := identity.EncodeRevocationPublic(pub)
	require.NoError(t, err)
	v := &revokingRig{rig: newRig(t), key: priv}
	v.revoke(t, 1)
	f := serveSigner(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/crl" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(*v.list.Load())
	}))
	v.chosen.Config.Spec.Signer = &config.SignerSection{}
	v.chosen.Config.Spec.API.RevocationPublicKey = pubPEM
	v.engine.cfg = v.chosen.Config
	v.server = api.New(api.Deps{
		Version:        "v1.2.3",
		BootID:         func() (string, error) { return "boot-1", nil },
		Uptime:         func() time.Duration { return time.Second },
		SignerSocket:   f.socket,
		RevocationFile: t.TempDir() + "/crl.json",
		Log:            slog.New(slog.NewTextHandler(v.logs, nil)),
	})
	t.Cleanup(func() { _ = v.server.Close() })
	return v
}

// issued returns a client holding a certificate the client CA signed, and the serial of it.
func (v *revokingRig) issued(t *testing.T) (*http.Client, string) {
	t.Helper()
	certPEM, keyPEM := pkitest.Issue(t, v.clientCA, "naru:admin")
	block, _ := pem.Decode([]byte(certPEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(v.serverCA.Cert)))
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: pool, MinVersion: tls.VersionTLS13},
		DisableKeepAlives: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, identity.SerialOf(cert)
}

// refused reports whether a request of c fails at the handshake or is answered 401 or 403.
func (v *revokingRig) refused(t *testing.T, c *http.Client) bool {
	t.Helper()
	resp, err := c.Get(v.url("/v1/version"))
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	return resp.StatusCode != http.StatusOK
}

func TestServer_ARevokedClientCertificateIsRefused(t *testing.T) {
	v := newRevokingRig(t)
	good, _ := v.issued(t)
	revoked, serial := v.issued(t)
	v.revoke(t, 2, serial)
	v.start(t)
	assert.True(t, v.refused(t, revoked), "the revoked certificate is refused")
	assert.False(t, v.refused(t, good), "a certificate the list does not name is accepted")
}

func TestServer_WithNoListToCheckAgainstEveryClientIsRefused(t *testing.T) {
	v := newRevokingRig(t)
	v.server = api.New(api.Deps{
		Version:      "v1.2.3",
		BootID:       func() (string, error) { return "boot-1", nil },
		Uptime:       func() time.Duration { return time.Second },
		SignerSocket: t.TempDir() + "/missing.sock",
		Log:          slog.New(slog.NewTextHandler(v.logs, nil)),
	})
	t.Cleanup(func() { _ = v.server.Close() })
	v.start(t)
	good, _ := v.issued(t)
	assert.True(t, v.refused(t, good), "the list cannot be fetched and none is kept: fail closed")
}

func TestServer_WithoutARevocationKeyOnlyTheClientCAIsChecked(t *testing.T) {
	v := newRevokingRig(t)
	v.chosen.Config.Spec.API.RevocationPublicKey = ""
	v.start(t)
	c, serial := v.issued(t)
	v.revoke(t, 2, serial)
	assert.False(t, v.refused(t, c), "no key, no revocation check")
	assert.Contains(t, v.logs.String(), "no spec.api.revocationPublicKey")
	assert.Nil(t, v.server.Revocations())
}

func TestServer_AReloadWithAKeyAndNoSourceIsRefused(t *testing.T) {
	r := newRig(t)
	r.start(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pubPEM, err := identity.EncodeRevocationPublic(pub)
	require.NoError(t, err)
	a := *r.chosen.Config.Spec.API
	a.RevocationPublicKey = pubPEM
	err = r.server.Reload(a)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revocationPublicKey")
}
