package fixture

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIssueServer_IsTrustedOnlyThroughTheRoot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, WriteRoot(dir))
	crt, key, err := IssueServer(dir)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(crt, key)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	root, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	require.NoError(t, err)
	require.True(t, pool.AppendCertsFromPEM(root))
	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	resp, err := trusting.Get(srv.URL)
	require.NoError(t, err)
	_ = resp.Body.Close()

	_, err = (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}}}).Get(srv.URL)
	assert.Error(t, err, "a client that does not trust the root refuses the server")
}
