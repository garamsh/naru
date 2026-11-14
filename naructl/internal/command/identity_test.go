package command_test

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
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

func readCSRFile(t *testing.T, path string) *x509.CertificateRequest {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	der := data
	if b, _ := pem.Decode(data); b != nil {
		der = b.Bytes
	}
	csr, err := x509.ParseCertificateRequest(der)
	require.NoError(t, err)
	require.NoError(t, csr.CheckSignature())
	return csr
}

func passFile(t *testing.T, pass string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pass")
	require.NoError(t, os.WriteFile(p, []byte(pass+"\n"), 0o600))
	return p
}

func TestOperatorRequest_WritesAKeyOnlyItsOwnerReadsAndARequestForIt(t *testing.T) {
	dir := t.TempDir()
	key, csrPath := filepath.Join(dir, "alice.key"), filepath.Join(dir, "alice.csr")

	text, err := run(t, "operator", "request", "--name", "alice", "--key-out", key, "--csr-out", csrPath)
	require.NoError(t, err, text)

	info, err := os.Stat(key)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.Equal(t, "alice", readCSRFile(t, csrPath).Subject.CommonName)
	keyPEM, err := os.ReadFile(key)
	require.NoError(t, err)
	assert.NotContains(t, text, string(keyPEM[:40]), "no key is printed")
	assert.NotContains(t, text, "PRIVATE KEY")

	_, err = run(t, "operator", "request", "--name", "alice", "--key-out", key, "--csr-out", csrPath)
	assert.Error(t, err, "a key already there is not overwritten")
}

func TestOperatorRequest_DERIsWhatNaruSignerInitReads(t *testing.T) {
	dir := t.TempDir()
	csrPath := filepath.Join(dir, "root.csr")

	_, err := run(t, "operator", "request", "--name", "root", "--key-out", filepath.Join(dir, "k"), "--csr-out", csrPath, "--der")
	require.NoError(t, err)

	data, err := os.ReadFile(csrPath)
	require.NoError(t, err)
	b, _ := pem.Decode(data)
	assert.Nil(t, b)
	_, err = x509.ParseCertificateRequest(data)
	assert.NoError(t, err)
}

func TestOperatorPerson_JoinsTheKeyAndItsCertificateIntoAFileThePassphraseOpensAndRemovesTheKey(t *testing.T) {
	dir := t.TempDir()
	key, csrPath, certPath, person := filepath.Join(dir, "k"), filepath.Join(dir, "c.csr"), filepath.Join(dir, "c.crt"), filepath.Join(dir, "p")
	mustRun(t, "operator", "request", "--name", "alice", "--key-out", key, "--csr-out", csrPath)
	csr := readCSRFile(t, csrPath)
	cert := issueFrom(t, caOf(t), "alice", identity.RoleOperator, csr.PublicKey)
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644))

	text, err := run(t, "operator", "person", "--key", key, "--cert", certPath, "--passphrase-file", passFile(t, "pw"), "--out", person)
	require.NoError(t, err, text)

	data, err := os.ReadFile(person)
	require.NoError(t, err)
	got, err := identity.DecryptPerson("pw", data)
	require.NoError(t, err)
	assert.Equal(t, cert.SerialNumber, got.Cert.SerialNumber)
	_, err = identity.DecryptPerson("other", data)
	assert.Error(t, err)
	_, err = os.Stat(key)
	assert.True(t, os.IsNotExist(err), "the plain key is removed")
	assert.NotContains(t, text, "PRIVATE KEY")
}

func TestOperatorPerson_RefusesACertificateOfAnotherKeyAndKeepsTheKey(t *testing.T) {
	dir := t.TempDir()
	key, csrPath, certPath := filepath.Join(dir, "k"), filepath.Join(dir, "c.csr"), filepath.Join(dir, "c.crt")
	mustRun(t, "operator", "request", "--name", "alice", "--key-out", key, "--csr-out", csrPath)
	_, otherPub := csrFile(t)
	cert := issueFrom(t, caOf(t), "alice", identity.RoleOperator, otherPub)
	require.NoError(t, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644))

	_, err := run(t, "operator", "person", "--key", key, "--cert", certPath, "--passphrase-file", passFile(t, "pw"), "--out", filepath.Join(dir, "p"))

	assert.Error(t, err)
	_, statErr := os.Stat(key)
	assert.NoError(t, statErr)
}

func TestOrchCSR_AsksForTheDNSNamesAndIPsAndRefusesNone(t *testing.T) {
	dir := t.TempDir()
	csrPath := filepath.Join(dir, "orch.csr")

	text, err := run(t, "orch", "csr", "--name", "orch-1", "--dns", "orch-1.naru, orch.naru", "--ip", "10.0.0.5,fd00::5",
		"--key-out", filepath.Join(dir, "orch.key"), "--csr-out", csrPath)
	require.NoError(t, err, text)

	csr := readCSRFile(t, csrPath)
	assert.Equal(t, []string{"orch-1.naru", "orch.naru"}, csr.DNSNames)
	require.Len(t, csr.IPAddresses, 2)
	assert.True(t, csr.IPAddresses[0].Equal(net.ParseIP("10.0.0.5")))
	assert.NotContains(t, text, "PRIVATE KEY")

	_, err = run(t, "orch", "csr", "--name", "x", "--key-out", filepath.Join(dir, "k2"), "--csr-out", filepath.Join(dir, "c2"))
	assert.Error(t, err)
	_, err = run(t, "orch", "csr", "--name", "x", "--ip", "not-an-ip", "--key-out", filepath.Join(dir, "k3"), "--csr-out", filepath.Join(dir, "c3"))
	assert.Error(t, err)
}

// serveSocket serves handler on a unix socket as naru-signer does.
func serveSocket(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.Listen("unix", socket)
	require.NoError(t, err)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Minute}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket
}

func rootPool(t *testing.T) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	return pool
}

func TestOrchIssue_SendsRootsRequestToIssueOrchAndWritesTheCertificate(t *testing.T) {
	var role identity.Role
	var req identity.IssueRequest
	socket := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		who, body, err := identity.VerifyRequest(raw, identity.ActionIssueOrch, rootPool(t), &identity.CRL{}, time.Now())
		if err != nil || r.URL.Path != "/v1/orch" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		role = who.Role
		_ = json.Unmarshal(body, &req)
		csr, _ := x509.ParseCertificateRequest(req.CSR)
		cert := issueFrom(t, caOf(t), req.Name, identity.RoleOrch, csr.PublicKey)
		_ = json.NewEncoder(w).Encode(identity.Issued{Certificate: cert.Raw, Serial: cert.SerialNumber.Text(16), NotAfter: cert.NotAfter})
	})
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	dir := t.TempDir()
	mustRun(t, "orch", "csr", "--name", "orch-1", "--dns", "orch-1", "--key-out", filepath.Join(dir, "k"), "--csr-out", filepath.Join(dir, "c"))
	out := filepath.Join(dir, "orch.crt")

	text, err := run(t, "orch", "issue", "--admin", admin, "--passphrase-file", pass, "--csr", filepath.Join(dir, "c"),
		"--name", "orch-1", "--socket", socket, "--out", out)
	require.NoError(t, err, text)

	assert.Equal(t, identity.RoleRoot, role)
	assert.Equal(t, "orch-1", req.Name)
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	b, _ := pem.Decode(data)
	require.NotNil(t, b)
	cert, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	assert.Equal(t, identity.RoleOrch, mustRole(t, cert))
}

func mustRole(t *testing.T, c *x509.Certificate) identity.Role {
	t.Helper()
	r, err := identity.RoleOf(c)
	require.NoError(t, err)
	return r
}

func TestSignerAuthoriseReplica_WritesRootsSignedRequestForTheHostAndItsJoinKey(t *testing.T) {
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	dir := t.TempDir()
	join := filepath.Join(dir, "join.pub")
	pub := []byte("0123456789abcdef0123456789abcdef")
	require.NoError(t, os.WriteFile(join, pub, 0o644))
	out := filepath.Join(dir, "authz")

	text, err := run(t, "signer", "authorise-replica", "--admin", admin, "--passphrase-file", pass, "--host-id", "ctl-2", "--join-key", join, "--out", out)
	require.NoError(t, err, text)

	signed, err := os.ReadFile(out)
	require.NoError(t, err)
	who, body, err := identity.VerifyRequest(signed, identity.ActionReplicate, rootPool(t), &identity.CRL{}, time.Now())
	require.NoError(t, err)
	assert.Equal(t, identity.RoleRoot, who.Role)
	var req identity.ReplicateRequest
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "ctl-2", req.HostID)
	assert.Equal(t, pub, req.X25519)
}

func TestSignerBundle_AsksOnAGETWithRootsRequestInTheHeaderAndWritesTheBundle(t *testing.T) {
	socket := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		signed, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Naru-Request"))
		if err != nil || r.Method != http.MethodGet || r.URL.Path != "/v1/bundle" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if _, _, err := identity.VerifyRequest(signed, identity.ActionBundle, rootPool(t), &identity.CRL{}, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("sealed-bundle"))
	})
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	out := filepath.Join(t.TempDir(), "bundle")

	text, err := run(t, "signer", "bundle", "--admin", admin, "--passphrase-file", pass, "--socket", socket, "--out", out)
	require.NoError(t, err, text)

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "sealed-bundle", string(got))
	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSignerRestore_PostsTheBundleWithRootsRequestNamingItsDigestAndTheKeyPaths(t *testing.T) {
	var got identity.RestoreBody
	var named identity.RestoreRequest
	socket := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil || r.URL.Path != "/v1/restore" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		_, body, err := identity.VerifyRequest(got.Authorisation, identity.ActionRestore, rootPool(t), &identity.CRL{}, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_ = json.Unmarshal(body, &named)
		_, _ = w.Write([]byte("{}"))
	})
	admin, pass := adminFile(t, identity.RoleRoot, "pw")
	bundle := filepath.Join(t.TempDir(), "bundle")
	require.NoError(t, os.WriteFile(bundle, []byte("sealed-bundle"), 0o600))

	text, err := run(t, "signer", "restore", "--admin", admin, "--passphrase-file", pass, "--bundle", bundle,
		"--recovery-key", "/keys/recovery.key", "--recovery-passphrase-file", "/keys/pass", "--socket", socket)
	require.NoError(t, err, text)

	sum := sha256.Sum256([]byte("sealed-bundle"))
	assert.Equal(t, hex.EncodeToString(sum[:]), named.BundleSHA256)
	assert.Equal(t, []byte("sealed-bundle"), got.Bundle)
	assert.Equal(t, "/keys/recovery.key", got.RecoveryKey)
	assert.Equal(t, "/keys/pass", got.PassphraseFile)
}

func TestSignerCommands_RefuseAnUnknownVerbAndAnAdminThatIsNotRoot(t *testing.T) {
	_, err := run(t, "signer", "nonsense")
	assert.Error(t, err)
	socket := serveSocket(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "refused", http.StatusForbidden) })
	admin, pass := adminFile(t, identity.RoleReader, "pw")
	_, err = run(t, "signer", "bundle", "--admin", admin, "--passphrase-file", pass, "--socket", socket, "--out", filepath.Join(t.TempDir(), "b"))
	assert.ErrorContains(t, err, "refused")
}

// recoveryFiles writes an encrypted recovery key and its passphrase file.
func recoveryFiles(t *testing.T, passphrase string) (key, pass string, rec identity.Recovery) {
	t.Helper()
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	enc, err := rec.Encrypt(passphrase)
	require.NoError(t, err)
	dir := t.TempDir()
	key, pass = filepath.Join(dir, "recovery.enc"), filepath.Join(dir, "pass")
	require.NoError(t, os.WriteFile(key, enc, 0o600))
	require.NoError(t, os.WriteFile(pass, []byte(passphrase+"\n"), 0o600))
	return key, pass, rec
}

func TestSignerReissueRoot_SendsARequestTheRecoveryKeySignedWithANonceAndWritesTheCertificate(t *testing.T) {
	keyFile, passFile, rec := recoveryFiles(t, "recovery pw")
	var nonces []string
	var csrSent []byte
	socket := serveSocket(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body, _, err := identity.VerifyRecoveryRequest(raw, identity.ActionReissueRoot, rec.Ed25519.Public().(ed25519.PublicKey), time.Now())
		if err != nil || r.URL.Path != "/v1/reissue-root" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		var req identity.ReissueRootRequest
		_ = json.Unmarshal(body, &req)
		nonces, csrSent = append(nonces, req.Nonce), req.CSR
		csr, _ := x509.ParseCertificateRequest(req.CSR)
		cert := issueFrom(t, caOf(t), "root", identity.RoleRoot, csr.PublicKey)
		_ = json.NewEncoder(w).Encode(identity.Issued{Certificate: cert.Raw, Serial: cert.SerialNumber.Text(16), NotAfter: cert.NotAfter})
	})
	csrPath, pub := csrFile(t)
	out := filepath.Join(t.TempDir(), "root.crt")

	text, err := run(t, "signer", "reissue-root", "--recovery-key", keyFile, "--passphrase-file", passFile, "--csr", csrPath, "--socket", socket, "--out", out)
	require.NoError(t, err, text)
	_, err = run(t, "signer", "reissue-root", "--recovery-key", keyFile, "--passphrase-file", passFile, "--csr", csrPath, "--socket", socket, "--out", out)
	require.NoError(t, err)

	require.Len(t, nonces, 2)
	assert.NotEqual(t, nonces[0], nonces[1], "each run asks with a nonce of its own")
	assert.Len(t, nonces[0], 2*identity.NonceBytes)
	sent, err := x509.ParseCertificateRequest(csrSent)
	require.NoError(t, err)
	assert.True(t, pub.Equal(sent.PublicKey), "the request names the new key")
	data, err := os.ReadFile(out)
	require.NoError(t, err)
	b, _ := pem.Decode(data)
	require.NotNil(t, b)
	cert, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	assert.Equal(t, identity.RoleRoot, mustRole(t, cert))
	assert.NotContains(t, text, "recovery pw")
}

func TestSignerReissueRoot_RefusesAWrongPassphraseWithoutCallingTheSigner(t *testing.T) {
	keyFile, _, _ := recoveryFiles(t, "recovery pw")
	wrong := passFile(t, "another pw")
	called := false
	socket := serveSocket(t, func(http.ResponseWriter, *http.Request) { called = true })
	csrPath, _ := csrFile(t)

	_, err := run(t, "signer", "reissue-root", "--recovery-key", keyFile, "--passphrase-file", wrong, "--csr", csrPath, "--socket", socket,
		"--out", filepath.Join(t.TempDir(), "root.crt"))

	assert.Error(t, err)
	assert.False(t, called)
}

func TestSignerReissueRoot_ReportsTheSignersRefusal(t *testing.T) {
	keyFile, passFile, _ := recoveryFiles(t, "recovery pw")
	socket := serveSocket(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "replayed", http.StatusForbidden) })
	csrPath, _ := csrFile(t)
	out := filepath.Join(t.TempDir(), "root.crt")

	_, err := run(t, "signer", "reissue-root", "--recovery-key", keyFile, "--passphrase-file", passFile, "--csr", csrPath, "--socket", socket, "--out", out)

	assert.ErrorContains(t, err, "replayed")
	assert.NoFileExists(t, out)
}
