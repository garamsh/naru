package command_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// fakeSigner serves one unix socket the way naru-signer does and records what it was asked.
type fakeSigner struct {
	socket string
	roots  *x509.CertPool
	got    map[string]json.RawMessage
	who    map[string]identity.Identity
	status int
}

func startFakeSigner(t *testing.T, roots *x509.CertPool) *fakeSigner {
	t.Helper()
	f := &fakeSigner{socket: shortSocketPath(t), roots: roots, got: map[string]json.RawMessage{}, who: map[string]identity.Identity{}, status: http.StatusOK}
	l, err := net.Listen("unix", f.socket)
	require.NoError(t, err)
	actions := map[string]string{"/v1/operator": identity.ActionIssueOperator, "/v1/revoke": identity.ActionRevoke}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		who, body, err := identity.VerifyRequest(raw, actions[r.URL.Path], f.roots, &identity.CRL{}, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		f.got[r.URL.Path], f.who[r.URL.Path] = body, who
		if f.status != http.StatusOK {
			http.Error(w, "refused by the fake", f.status)
			return
		}
		if r.URL.Path == "/v1/revoke" {
			_, _ = w.Write([]byte("crl"))
			return
		}
		var req identity.IssueRequest
		_ = json.Unmarshal(body, &req)
		csr, _ := x509.ParseCertificateRequest(req.CSR)
		cert := issueFrom(t, caOf(t), req.Name, req.Role, csr.PublicKey)
		_ = json.NewEncoder(w).Encode(identity.Issued{Certificate: cert.Raw, Serial: cert.SerialNumber.Text(16), NotAfter: cert.NotAfter})
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

var testCA *person

type person struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

func caOf(t *testing.T) person {
	t.Helper()
	if testCA != nil {
		return *testCA
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-operator-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	testCA = &person{key: key, cert: cert}
	return *testCA
}

func issueFrom(t *testing.T, ca person, cn string, role identity.Role, pub any) *x509.Certificate {
	t.Helper()
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial.Add(serial, big.NewInt(2)),
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: []string{string(role)}},
		NotBefore:    time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

// adminFile writes an encrypted person file of role and returns it with a passphrase file.
func adminFile(t *testing.T, role identity.Role, passphrase string) (file, passFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cert := issueFrom(t, caOf(t), "admin", role, &key.PublicKey)
	enc, err := identity.EncryptPerson(passphrase, identity.Person{Key: key, Cert: cert})
	require.NoError(t, err)
	dir := t.TempDir()
	file, passFile = filepath.Join(dir, "admin.person"), filepath.Join(dir, "pass")
	require.NoError(t, os.WriteFile(file, enc, 0o600))
	require.NoError(t, os.WriteFile(passFile, []byte(passphrase+"\n"), 0o600))
	return file, passFile
}

func csrFile(t *testing.T) (path string, pub *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "x"}}, key)
	require.NoError(t, err)
	path = filepath.Join(t.TempDir(), "x.csr")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), 0o600))
	return path, &key.PublicKey
}

func TestOperatorAdd_SendsARootSignedRequestForTheCSRAndWritesTheCertificate(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	csr, pub := csrFile(t)
	out := filepath.Join(t.TempDir(), "alice.crt")

	text, err := run(t, "operator", "add", "--admin", admin, "--passphrase-file", pass, "--csr", csr,
		"--name", "alice", "--role", "reader", "--socket", f.socket, "--out", out)
	require.NoError(t, err, text)

	assert.Equal(t, identity.RoleRoot, f.who["/v1/operator"].Role, "the request is signed with the admin file's key and certificate")
	var req identity.IssueRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/operator"], &req))
	assert.Equal(t, "alice", req.Name)
	assert.Equal(t, identity.RoleReader, req.Role)

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, "alice", cert.Subject.CommonName)
	assert.True(t, cert.PublicKey.(*ecdsa.PublicKey).Equal(pub), "the certificate is for the key of the CSR")
	assert.Contains(t, text, "serial")
}

func TestOperatorAdd_StopsBeforeTheSignerOnABadPassphraseAndReportsItsRefusal(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, _ := adminFile(t, identity.RoleRoot, "pw")
	csr, _ := csrFile(t)
	wrong := filepath.Join(t.TempDir(), "wrong")
	require.NoError(t, os.WriteFile(wrong, []byte("nope\n"), 0o600))
	args := func(pass string) []string {
		return []string{"operator", "add", "--admin", admin, "--passphrase-file", pass, "--csr", csr, "--name", "alice", "--socket", f.socket, "--out", filepath.Join(t.TempDir(), "c")}
	}

	_, err := run(t, args(wrong)...)
	require.Error(t, err)
	assert.Empty(t, f.got, "nothing was sent")

	f.status = http.StatusForbidden
	good := filepath.Join(t.TempDir(), "good")
	require.NoError(t, os.WriteFile(good, []byte("pw\n"), 0o600))
	text, err := run(t, args(good)...)
	require.Error(t, err)
	assert.Contains(t, text, "refused")
	assert.Contains(t, text, "refused by the fake")
}

func TestOperatorRevoke_SendsTheSerialSignedByTheAdmin(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleOperator, "pw")

	text, err := run(t, "operator", "revoke", "--admin", admin, "--passphrase-file", pass, "--serial", "ABCD1234", "--socket", f.socket)
	require.NoError(t, err, text)
	var req identity.RevokeRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/revoke"], &req))
	assert.Equal(t, "abcd1234", req.Serial)
	assert.Equal(t, identity.RoleOperator, f.who["/v1/revoke"].Role)

	_, err = run(t, "operator", "revoke", "--admin", admin, "--passphrase-file", pass, "--socket", f.socket)
	assert.Error(t, err, "the serial is required")
	_, err = run(t, "operator", "nonsense")
	assert.Error(t, err)
}
