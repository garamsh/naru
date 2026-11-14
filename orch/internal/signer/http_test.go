package signer_test

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/signer"
)

// serve listens on a unix socket with h and returns a client of it.
func serve(t *testing.T, h http.Handler) *signer.Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
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
	require.Equal(t, "CERT", string(cert))
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

func TestCRLAndOrchCertificate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/crl", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("signed-crl")) })
	mux.HandleFunc("POST /v1/orch", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string][]byte{"certificate": []byte("C"), "ca": []byte("A")})
	})
	c := serve(t, mux)
	crl, err := c.CRL(t.Context())
	require.NoError(t, err)
	require.Equal(t, "signed-crl", string(crl))
	got, err := c.IssueOrch(t.Context(), []byte("csr"))
	require.NoError(t, err)
	require.Equal(t, "C", string(got.Certificate))
	require.Equal(t, "A", string(got.CA))
}
