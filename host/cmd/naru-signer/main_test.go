package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadCSR_TakesPEMAndDER(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "root"}}, key)
	require.NoError(t, err)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}

	got, err := readCSR(write("der", der))
	require.NoError(t, err)
	assert.Equal(t, der, got, "DER is returned as it is")

	got, err = readCSR(write("pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})))
	require.NoError(t, err)
	assert.Equal(t, der, got, "PEM is decoded to its DER")

	_, err = readCSR(write("other", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	assert.Error(t, err, "a PEM of another type is refused")

	_, err = readCSR(filepath.Join(dir, "missing"))
	assert.Error(t, err)
}

func TestRunJoinKey_WritesThePublicKeyAndPrintsItOnTheConsole(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	out := filepath.Join(t.TempDir(), "join.pub")
	r, w, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = w
	runErr := runJoinKey(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"--dir", dir, "--out", out})
	os.Stdout = stdout
	require.NoError(t, w.Close())
	console, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, runErr)

	pub, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Len(t, pub, 32)
	assert.Contains(t, string(console), base64.StdEncoding.EncodeToString(pub), "the key an operator authorises is on the console")
	assert.FileExists(t, filepath.Join(dir, "join.key"), "the private half stays in the directory")
}
