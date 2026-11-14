package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
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

func TestRunInit_PrintsTheRootCertificateAndTheRevocationKeyAsPEMOnTheConsole(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	work := t.TempDir()
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	recPub, err := rec.Public().MarshalPublic()
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "root", OrganizationalUnit: []string{"root"}}}, key)
	require.NoError(t, err)
	write := func(name string, b []byte) string {
		p := filepath.Join(work, name)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}
	args := []string{"--dir", dir, "--recovery-public", write("rec.pub", recPub), "--root-csr", write("root.csr", csr),
		"--root-cert", filepath.Join(work, "root.crt"), "--revocation-public-key", filepath.Join(work, "rev.pub")}

	r, w, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = w
	runErr := runInit(slog.New(slog.NewTextHandler(io.Discard, nil)), args)
	os.Stdout = stdout
	require.NoError(t, w.Close())
	console, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, runErr)

	block, rest := pem.Decode(console[len("root certificate:\n"):])
	require.NotNil(t, block, "the root certificate is PEM on the console")
	assert.Equal(t, "CERTIFICATE", block.Type)
	_, err = x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	onDisk, err := os.ReadFile(filepath.Join(work, "root.crt"))
	require.NoError(t, err)
	assert.Equal(t, onDisk, block.Bytes, "the console shows the certificate the file holds")
	assert.Contains(t, string(rest), "-----BEGIN PUBLIC KEY-----", "the revocation key follows")
}

func TestRunServe_ABlankDirectoryServesItsSocketSoThatARestoreCanReachIt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	short, err := os.MkdirTemp("", "sg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	sock := filepath.Join(short, "s.sock")

	done := make(chan error, 1)
	go func() {
		done <- runServe(slog.New(slog.NewTextHandler(io.Discard, nil)), []string{"--dir", dir, "--socket", sock, "--peer-listen", "127.0.0.1:0"})
	}()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	require.Eventually(t, func() bool {
		resp, err := client.Get("http://signer/v1/crl")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusConflict
	}, 5*time.Second, 50*time.Millisecond, "a blank signer answers on its socket, saying it holds no authorities")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("the signer did not end on SIGTERM")
	}
}
