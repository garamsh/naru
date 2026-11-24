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

func callerArgs(f *fakeSigner, admin, pass, dir string, extra ...string) []string {
	return append([]string{"caller", "credential", "--admin", admin, "--passphrase-file", pass, "--socket", f.socket,
		"--cert-out", filepath.Join(dir, "caller.crt"), "--key-out", filepath.Join(dir, "caller.key")}, extra...)
}

func TestCallerCredential_AsksTheSignerForTheCallerAndWritesACertificateOfTheKeyItMade(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	dir := t.TempDir()

	text, err := run(t, callerArgs(f, admin, pass, dir, "--name", "garam", "--days", "30")...)
	require.NoError(t, err, text)
	assert.Contains(t, text, "issued a caller certificate for garam")

	assert.Equal(t, identity.RoleRoot, f.who["/v1/caller-issue"].Role, "the request is signed with the admin file's key")
	var req identity.CallerRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/caller-issue"], &req))
	assert.Equal(t, "garam", req.Name)
	assert.Equal(t, 30, req.Days)
	csr, err := x509.ParseCertificateRequest(req.CSR)
	require.NoError(t, err)

	certPEM, err := os.ReadFile(filepath.Join(dir, "caller.crt"))
	require.NoError(t, err)
	cb, _ := pem.Decode(certPEM)
	require.NotNil(t, cb)
	cert, err := x509.ParseCertificate(cb.Bytes)
	require.NoError(t, err)
	assert.Equal(t, "garam", cert.Subject.CommonName)
	assert.Equal(t, []string{"caller"}, cert.Subject.OrganizationalUnit)

	keyPEM, err := os.ReadFile(filepath.Join(dir, "caller.key"))
	require.NoError(t, err)
	kb, _ := pem.Decode(keyPEM)
	require.NotNil(t, kb)
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	require.NoError(t, err)
	pub := &key.(*ecdsa.PrivateKey).PublicKey
	assert.True(t, csr.PublicKey.(*ecdsa.PublicKey).Equal(pub), "the request sent is for the key written")
	assert.True(t, cert.PublicKey.(*ecdsa.PublicKey).Equal(pub), "the certificate is for the key written beside it")
	fi, err := os.Stat(filepath.Join(dir, "caller.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
}

func TestCallerCredential_RefusesAnInvalidNameBeforeAnythingIsSent(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")

	for _, bad := range []string{"Garam", "a.b", "-x", "x_y", "", "a/../b", "root@x"} {
		dir := t.TempDir()
		_, err := run(t, callerArgs(f, admin, pass, dir, "--name", bad)...)
		assert.Error(t, err, "name %q", bad)
		assert.Empty(t, f.got, "name %q: nothing was sent", bad)
		assert.NoFileExists(t, filepath.Join(dir, "caller.key"), "name %q: no key was left", bad)
	}
	text, err := run(t, callerArgs(f, admin, pass, t.TempDir(), "--name", "use-1")...)
	require.NoError(t, err, "the control: a DNS label is accepted: %s", text)
	assert.NotEmpty(t, f.got["/v1/caller-issue"])
}

func TestCallerCredential_RefusesMoreThanNinetyDaysAndNeverWritesOverAFile(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")

	for _, days := range []string{"91", "0", "-1"} {
		_, err := run(t, callerArgs(f, admin, pass, t.TempDir(), "--name", "garam", "--days", days)...)
		assert.Error(t, err, "--days %s", days)
		assert.Empty(t, f.got, "--days %s: nothing was sent", days)
	}
	text, err := run(t, callerArgs(f, admin, pass, t.TempDir(), "--name", "garam")...)
	require.NoError(t, err, "the control: the default is accepted: %s", text)
	var req identity.CallerRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/caller-issue"], &req))
	assert.Equal(t, 90, req.Days, "the default is 90 days")

	for _, file := range []string{"caller.key", "caller.crt"} {
		taken := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(taken, file), []byte("mine"), 0o600))
		calls := 0
		f.onCall = func(string) { calls++ }
		_, err = run(t, callerArgs(f, admin, pass, taken, "--name", "garam")...)
		assert.Error(t, err, file)
		got, _ := os.ReadFile(filepath.Join(taken, file))
		assert.Equal(t, "mine", string(got), "an existing %s is not replaced", file)
		assert.Zero(t, calls, "%s: nothing was sent: the signer would issue a certificate nobody could keep", file)
	}
}

func TestCallerCredential_ReportsTheSignersRefusalAndWritesNothing(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	f.status = 403
	admin, pass := adminFile(t, identity.RoleOperator, "pw")
	dir := t.TempDir()

	text, err := run(t, callerArgs(f, admin, pass, dir, "--name", "garam")...)
	require.Error(t, err)
	assert.Contains(t, text, "refused by the fake")
	assert.NoFileExists(t, filepath.Join(dir, "caller.key"), "a refused request leaves no key")
	assert.NoFileExists(t, filepath.Join(dir, "caller.crt"))
}

func TestCallerToken_AsksTheSignerAsRootAndWritesTheTokenOnlyToTheFile(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	out := filepath.Join(t.TempDir(), "token")

	text, err := run(t, "caller", "token", "--name", "garam-bot", "--ttl", "2h", "--admin", admin, "--passphrase-file", pass, "--socket", f.socket, "--out", out)
	require.NoError(t, err, text)
	assert.Equal(t, identity.RoleRoot, f.who["/v1/caller-token"].Role, "the request is signed with the admin file's key")
	var req identity.CallerTokenRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/caller-token"], &req))
	assert.Equal(t, identity.CallerTokenRequest{Name: "garam-bot", TTLSeconds: 7200}, req)
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "naru-ct-for-garam-bot\n", string(b))
	fi, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	assert.NotContains(t, text, "naru-ct-", "the token is printed nowhere")

	for name, extra := range map[string][]string{
		"the file exists": {"--name", "garam-bot", "--out", out},
		"a bad name":      {"--name", "Garam", "--out", filepath.Join(t.TempDir(), "t")},
		"over 24 hours":   {"--name", "garam-bot", "--ttl", "25h", "--out", filepath.Join(t.TempDir(), "t")},
	} {
		delete(f.got, "/v1/caller-token")
		_, err := run(t, append([]string{"caller", "token", "--admin", admin, "--passphrase-file", pass, "--socket", f.socket}, extra...)...)
		assert.Error(t, err, name)
		assert.NotContains(t, f.got, "/v1/caller-token", "%s: nothing is sent", name)
	}
	b, err = os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "naru-ct-for-garam-bot\n", string(b), "the file is not written over")
}
