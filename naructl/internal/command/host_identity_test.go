package command_test

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// operatorHost serves the host API's GET /v1/host over mutual TLS as a seed-joined host does: its
// serving pair is the host authority's, and it verifies its clients against the operator authority
// the signer issues people's certificates from. It returns its address and the role of the last
// client certificate it answered.
func operatorHost(t *testing.T, dep deployment) (string, *atomic.Value) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	var seen atomic.Value
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, err := identity.RoleOf(r.TLS.VerifiedChains[0][0])
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		seen.Store(string(role))
		_, _ = w.Write([]byte(`{"region":"eu-1"}`))
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{servingPair(t, dep, net.ParseIP("127.0.0.1"))}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), &seen
}

func TestHost_ReadsASeedJoinedHostWithAPersonTheSignerIssuedACertificateTo(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	addr, seen := operatorHost(t, dep)
	hostCA := filepath.Join(dep.ca, "host", "ca.pem")

	person, pass := adminFile(t, identity.RoleOperator, "hunter2")
	out, err := run(t, "host", "--identity", person, "--passphrase-file", pass, "--host-ca", hostCA, "--endpoint", addr)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"region": "eu-1"`)
	assert.Equal(t, "operator", seen.Load(), "the host saw the person's certificate")

	// The controls: a certificate of another authority is refused by the handshake, and the caller is
	// named once.
	other := t.TempDir()
	foreign, foreignPass := foreignPerson(t, other)
	_, err = run(t, "host", "--identity", foreign, "--passphrase-file", foreignPass, "--host-ca", hostCA, "--endpoint", addr)
	assert.Error(t, err, "a person of an authority the host does not trust")
	_, err = run(t, "host", "--identity", person, "--passphrase-file", pass, "--host-ca", hostCA, "--endpoint", addr, "--config", filepath.Join(other, "c.yaml"))
	assert.ErrorContains(t, err, "give one")
	_, err = run(t, "host", "--identity", person, "--passphrase-file", pass, "--endpoint", addr)
	assert.ErrorContains(t, err, "--host-ca is required")
	_, err = run(t, "host", "--passphrase-file", pass, "--endpoint", addr)
	assert.Error(t, err, "--passphrase-file has no meaning without --identity")
	wrong := filepath.Join(other, "wrong")
	require.NoError(t, os.WriteFile(wrong, []byte("not it\n"), 0o600))
	_, err = run(t, "host", "--identity", person, "--passphrase-file", wrong, "--host-ca", hostCA, "--endpoint", addr)
	assert.Error(t, err, "the passphrase opens the person file")
}

// foreignPerson is a person file of a certificate issued by an authority the host does not trust.
func foreignPerson(t *testing.T, dir string) (file, passFile string) {
	t.Helper()
	saved := testCA
	testCA = nil
	t.Cleanup(func() { testCA = saved })
	file, passFile = adminFile(t, identity.RoleOperator, "other")
	testCA = saved
	return file, passFile
}
