package command_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

func ccmArgs(f *fakeSigner, admin, pass, dir string, extra ...string) []string {
	return append([]string{"ccm", "credential", "--admin", admin, "--passphrase-file", pass, "--socket", f.socket,
		"--cert-out", filepath.Join(dir, "tls.crt"), "--key-out", filepath.Join(dir, "tls.key")}, extra...)
}

func TestCCMCredential_AsksTheSignerForTheNetworksExposerAndWritesACertificateOfTheKeyItMade(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleOperator, "pw")
	dir := t.TempDir()

	text, err := run(t, ccmArgs(f, admin, pass, dir, "--network", "prod", "--days", "30")...)
	require.NoError(t, err, text)

	assert.Equal(t, identity.RoleOperator, f.who["/v1/exposer"].Role, "the request is signed with the admin file's key")
	var req identity.ExposerRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/exposer"], &req))
	assert.Equal(t, "prod", req.Network)
	assert.Equal(t, 30, req.Days)

	certPEM, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	require.NoError(t, err)
	cb, _ := pem.Decode(certPEM)
	require.NotNil(t, cb)
	cert, err := x509.ParseCertificate(cb.Bytes)
	require.NoError(t, err)
	assert.Equal(t, "exposer.prod", cert.Subject.CommonName)
	assert.Equal(t, []string{"exposer"}, cert.Subject.OrganizationalUnit)

	keyPEM, err := os.ReadFile(filepath.Join(dir, "tls.key"))
	require.NoError(t, err)
	kb, _ := pem.Decode(keyPEM)
	require.NotNil(t, kb)
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	require.NoError(t, err)
	assert.True(t, cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.(*ecdsa.PrivateKey).PublicKey), "the certificate is for the key written beside it")
	fi, err := os.Stat(filepath.Join(dir, "tls.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestCCMCredential_RefusesAnInvalidNetworkNameBeforeAnythingIsSent(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleOperator, "pw")

	for _, bad := range []string{"Prod", "a.b", "-x", "x_y", "", "a/../b", "exposer.prod"} {
		dir := t.TempDir()
		_, err := run(t, ccmArgs(f, admin, pass, dir, "--network", bad)...)
		assert.Error(t, err, "network %q", bad)
		assert.Empty(t, f.got, "network %q: nothing was sent", bad)
		assert.NoFileExists(t, filepath.Join(dir, "tls.key"), "network %q: no key was left", bad)
	}
	text, err := run(t, ccmArgs(f, admin, pass, t.TempDir(), "--network", "prod-1")...)
	require.NoError(t, err, "the control: a DNS label is accepted: %s", text)
	assert.NotEmpty(t, f.got["/v1/exposer"])
}

func TestCCMCredential_RefusesMoreThanNinetyDaysAndNeverWritesOverAFile(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleOperator, "pw")

	for _, days := range []string{"91", "0", "-1"} {
		_, err := run(t, ccmArgs(f, admin, pass, t.TempDir(), "--network", "prod", "--days", days)...)
		assert.Error(t, err, "--days %s", days)
		assert.Empty(t, f.got, "--days %s: nothing was sent", days)
	}
	text, err := run(t, ccmArgs(f, admin, pass, t.TempDir(), "--network", "prod", "--days", "90")...)
	require.NoError(t, err, "the control: 90 days is accepted: %s", text)

	taken := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(taken, "tls.key"), []byte("mine"), 0o600))
	calls := 0
	f.onCall = func(string) { calls++ }
	_, err = run(t, ccmArgs(f, admin, pass, taken, "--network", "prod")...)
	assert.Error(t, err)
	got, _ := os.ReadFile(filepath.Join(taken, "tls.key"))
	assert.Equal(t, "mine", string(got), "an existing key is not replaced")
	assert.Zero(t, calls, "nothing was sent: the signer would issue a certificate nobody could keep")
}

func TestCCMCredential_ReportsTheSignersRefusalAndWritesNothing(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	f.status = 403
	admin, pass := adminFile(t, identity.RoleReader, "pw")
	dir := t.TempDir()

	text, err := run(t, ccmArgs(f, admin, pass, dir, "--network", "prod")...)
	require.Error(t, err)
	assert.Contains(t, text, "refused by the fake")
	assert.NoFileExists(t, filepath.Join(dir, "tls.key"), "a refused request leaves no key")
	assert.NoFileExists(t, filepath.Join(dir, "tls.crt"))
}
