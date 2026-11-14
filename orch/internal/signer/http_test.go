package signer_test

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/signer"
)

// serve listens on a unix socket with h and returns a client of it.
func serve(t *testing.T, h http.Handler) *signer.Client {
	t.Helper()
	// t.TempDir carries the test's name, which with a long TMPDIR passes the 107 bytes of a socket path.
	dir, err := os.MkdirTemp("", "s")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return signer.New(path)
}

func TestIssueHostSendsTheStatementAndTheRequestAndReturnsTheCertificate(t *testing.T) {
	var got struct{ Statement, CSR []byte }
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "POST /v1/host", r.Method+" "+r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		_ = json.NewEncoder(w).Encode(map[string][]byte{"certificate": []byte("CERT")})
	}))
	cert, err := c.IssueHost(t.Context(), []byte("stmt"), []byte("csr"))
	require.NoError(t, err)
	block, rest := pem.Decode(cert)
	require.NotNil(t, block, "the signer's DER certificate is returned as PEM")
	require.Empty(t, rest)
	require.Equal(t, "CERTIFICATE", block.Type)
	require.Equal(t, "CERT", string(block.Bytes))
	require.Equal(t, "stmt", string(got.Statement))
	require.Equal(t, "csr", string(got.CSR))
}

func TestARefusalIsDistinguishedFromAFailure(t *testing.T) {
	status := http.StatusForbidden
	c := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", status)
	}))
	_, err := c.IssueHost(t.Context(), []byte("s"), []byte("c"))
	require.ErrorIs(t, err, inventory.ErrSignerRefused)

	status = http.StatusInternalServerError
	_, err = c.IssueHost(t.Context(), []byte("s"), []byte("c"))
	require.Error(t, err)
	require.NotErrorIs(t, err, inventory.ErrSignerRefused, "a failure of the signer is not the operator's refusal")
}

func TestCRL(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/crl", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("signed-crl")) })
	c := serve(t, mux)
	crl, err := c.CRL(t.Context())
	require.NoError(t, err)
	require.Equal(t, "signed-crl", string(crl))
}

func TestPushCRLSendsTheListAsTheBodyAndReturnsTheListTheSignerHolds(t *testing.T) {
	var got []byte
	status := http.StatusOK
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/crl", func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if status != http.StatusOK {
			http.Error(w, "no", status)
			return
		}
		_, _ = w.Write([]byte("held-crl"))
	})
	c := serve(t, mux)
	held, err := c.PushCRL(t.Context(), []byte("pushed-crl"))
	require.NoError(t, err)
	require.Equal(t, "pushed-crl", string(got), "the signed list goes as it is, not wrapped")
	require.Equal(t, "held-crl", string(held))

	status = http.StatusForbidden
	_, err = c.PushCRL(t.Context(), []byte("forged"))
	require.ErrorIs(t, err, inventory.ErrSignerRefused)
}
