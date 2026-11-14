package signer_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type person struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return k
}

func csrFor(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ignored"}}, key)
	require.NoError(t, err)
	return der
}

func keyHash(t *testing.T, csr []byte) string {
	t.Helper()
	r, err := x509.ParseCertificateRequest(csr)
	require.NoError(t, err)
	sum := sha256.Sum256(r.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func privateDir(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "signer")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o700))
	return dir
}

func openAt(t *testing.T, dir string) *signer.Signer {
	t.Helper()
	s, err := signer.Open(signer.Options{Dir: dir, Log: quiet})
	require.NoError(t, err)
	return s
}

// env is an initialised signer in a temporary root, with a root identity and the recovery key's files.
type env struct {
	t            *testing.T
	root         string
	dir          string
	s            *signer.Signer
	srv          *httptest.Server
	rootID       person
	recoveryFile string
	passFile     string
	recovery     identity.Recovery
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, root: t.TempDir()}
	e.dir = privateDir(t, e.root)
	e.s = openAt(t, e.dir)

	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	e.recovery = rec
	e.passFile = filepath.Join(e.root, "pass")
	require.NoError(t, os.WriteFile(e.passFile, []byte("recovery passphrase\n"), 0o600))
	sealed, err := rec.Encrypt("recovery passphrase")
	require.NoError(t, err)
	e.recoveryFile = filepath.Join(e.root, "recovery.enc")
	require.NoError(t, os.WriteFile(e.recoveryFile, sealed, 0o600))
	pub, err := rec.Public().MarshalPublic()
	require.NoError(t, err)
	parsed, err := identity.ParseRecoveryPublic(pub)
	require.NoError(t, err)

	key := newKey(t)
	issued, err := e.s.Init(signer.InitOptions{RecoveryPublic: parsed, RootCSR: csrFor(t, key)})
	require.NoError(t, err)
	e.rootID = person{key: key, cert: parseCert(t, issued.Certificate)}
	e.srv = httptest.NewServer(e.s.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func parseCert(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return c
}

func (e *env) signed(p person, action string, body any) []byte {
	e.t.Helper()
	b, err := identity.SignRequest(action, body, time.Now().Add(time.Minute), p.key, p.cert)
	require.NoError(e.t, err)
	return b
}

func (e *env) do(method, path string, body []byte, header ...string) (int, []byte) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, bytes.NewReader(body))
	require.NoError(e.t, err)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	require.NoError(e.t, err)
	return resp.StatusCode, out
}

// issue has root issue a certificate of role for a fresh key through the API.
func (e *env) issue(role identity.Role, name string) person {
	e.t.Helper()
	key := newKey(e.t)
	status, body := e.do(http.MethodPost, "/v1/operator", e.signed(e.rootID, identity.ActionIssueOperator,
		identity.IssueRequest{Name: name, Role: role, CSR: csrFor(e.t, key)}))
	require.Equal(e.t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(e.t, json.Unmarshal(body, &issued))
	return person{key: key, cert: parseCert(e.t, issued.Certificate)}
}

func (e *env) authorities() identity.Authorities {
	e.t.Helper()
	status, body := e.do(http.MethodGet, "/v1/ca", nil)
	require.Equal(e.t, http.StatusOK, status)
	var a identity.Authorities
	require.NoError(e.t, json.Unmarshal(body, &a))
	return a
}

func (e *env) revoke(by person, serial string) (int, []byte) {
	return e.do(http.MethodPost, "/v1/revoke", e.signed(by, identity.ActionRevoke, identity.RevokeRequest{Serial: serial}))
}

func (e *env) hostRequest(by person, hostID string, hostKey *ecdsa.PrivateKey, expires time.Time) []byte {
	e.t.Helper()
	csr := csrFor(e.t, hostKey)
	stmt, err := identity.SignAdmission(identity.Admission{HostID: hostID, HostKeySHA256: keyHash(e.t, csr), Region: "kr", ExpiresAt: expires}, by.key, by.cert)
	require.NoError(e.t, err)
	b, err := json.Marshal(identity.HostRequest{Statement: stmt, CSR: csr})
	require.NoError(e.t, err)
	return b
}

func (e *env) hostCert(hostID string) *x509.Certificate {
	e.t.Helper()
	op := e.issue(identity.RoleOperator, "alice")
	status, body := e.do(http.MethodPost, "/v1/host", e.hostRequest(op, hostID, newKey(e.t), time.Now().Add(time.Hour)))
	require.Equal(e.t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(e.t, json.Unmarshal(body, &issued))
	return parseCert(e.t, issued.Certificate)
}

func roleOf(t *testing.T, c *x509.Certificate) identity.Role {
	t.Helper()
	r, err := identity.RoleOf(c)
	require.NoError(t, err)
	return r
}

func poolOf(t *testing.T, der []byte) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	p.AddCert(parseCert(t, der))
	return p
}

func TestOpen_RefusesADirectoryOthersCanReach(t *testing.T) {
	root := t.TempDir()
	loose := filepath.Join(root, "loose")
	require.NoError(t, os.Mkdir(loose, 0o755))
	require.NoError(t, os.Chmod(loose, 0o755))
	_, err := signer.Open(signer.Options{Dir: loose, Log: quiet})
	require.ErrorIs(t, err, signer.ErrDirectory)

	s := openAt(t, privateDir(t, root))
	assert.False(t, s.Initialised(), "a blank 0700 directory opens, holding nothing")

	_, err = signer.Open(signer.Options{Dir: filepath.Join(root, "missing"), Log: quiet})
	require.ErrorIs(t, err, signer.ErrDirectory)
}

func TestOpen_RefusesAPartialDirectoryAndAKeyOthersCanRead(t *testing.T) {
	e := newEnv(t)
	files, err := os.ReadDir(e.dir)
	require.NoError(t, err)
	require.NotEmpty(t, files)

	partial := privateDir(t, t.TempDir())
	src, err := os.ReadFile(filepath.Join(e.dir, "host-ca.crt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(partial, "host-ca.crt"), src, 0o600))
	_, err = signer.Open(signer.Options{Dir: partial, Log: quiet})
	require.Error(t, err, "one authority without the others is a damaged directory")

	require.NoError(t, os.Chmod(filepath.Join(e.dir, "operator-ca.key"), 0o644))
	_, err = signer.Open(signer.Options{Dir: e.dir, Log: quiet})
	require.Error(t, err, "a key another user can read is refused")
	require.NoError(t, os.Chmod(filepath.Join(e.dir, "operator-ca.key"), 0o600))
	openAt(t, e.dir)
}

func TestInit_WritesEveryKeyOwnerOnlyAndRefusesASecondInit(t *testing.T) {
	e := newEnv(t)
	entries, err := os.ReadDir(e.dir)
	require.NoError(t, err)
	var keys int
	for _, ent := range entries {
		info, err := ent.Info()
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), ent.Name())
		if strings.HasSuffix(ent.Name(), ".key") {
			keys++
		}
	}
	assert.Equal(t, 4, keys, "the host, operator and orch authorities and the revocation key")

	_, err = e.s.Init(signer.InitOptions{RootCSR: csrFor(t, newKey(t))})
	require.ErrorIs(t, err, signer.ErrInitialised)
}

func TestHost_IssuedForAnOperatorsStatementAndTheKeyItNames(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	hostKey := newKey(t)

	status, body := e.do(http.MethodPost, "/v1/host", e.hostRequest(op, "host-1", hostKey, time.Now().Add(time.Hour)))
	require.Equal(t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	cert := parseCert(t, issued.Certificate)

	assert.Equal(t, "host-1", cert.Subject.CommonName)
	assert.Equal(t, identity.RoleHost, roleOf(t, cert))
	assert.True(t, cert.PublicKey.(*ecdsa.PublicKey).Equal(&hostKey.PublicKey))
	assert.ElementsMatch(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, cert.ExtKeyUsage)
	assert.WithinDuration(t, time.Now().AddDate(1, 0, 0), cert.NotAfter, time.Minute)
	for _, usage := range [][]x509.ExtKeyUsage{{x509.ExtKeyUsageServerAuth}, {x509.ExtKeyUsageClientAuth}} {
		_, err := cert.Verify(x509.VerifyOptions{Roots: poolOf(t, e.authorities().Host), KeyUsages: usage})
		assert.NoError(t, err)
	}
}

func TestHost_RefusedForAnythingButAnOperatorsStatementOverTheRequestsOwnKey(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	reader := e.issue(identity.RoleReader, "bob")
	revoked := e.issue(identity.RoleOperator, "mallory")
	status, _ := e.revoke(e.rootID, strings.ToLower(revoked.cert.SerialNumber.Text(16)))
	require.Equal(t, http.StatusOK, status)
	future := time.Now().Add(time.Hour)

	otherKey := newKey(t)
	mismatched := func() []byte {
		stmtCSR := csrFor(t, newKey(t))
		stmt, err := identity.SignAdmission(identity.Admission{HostID: "host-1", HostKeySHA256: keyHash(t, stmtCSR), ExpiresAt: future}, op.key, op.cert)
		require.NoError(t, err)
		b, err := json.Marshal(identity.HostRequest{Statement: stmt, CSR: csrFor(t, otherKey)})
		require.NoError(t, err)
		return b
	}()

	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"a reader's statement", e.hostRequest(reader, "host-1", newKey(t), future), http.StatusForbidden},
		{"a revoked operator's statement", e.hostRequest(revoked, "host-1", newKey(t), future), http.StatusForbidden},
		{"an expired statement", e.hostRequest(op, "host-1", newKey(t), time.Now().Add(-time.Minute)), http.StatusForbidden},
		{"a request over another key than the statement names", mismatched, http.StatusBadRequest},
		{"a host ID that is not a name", e.hostRequest(op, "../etc", newKey(t), future), http.StatusBadRequest},
		{"garbage", []byte("not json"), http.StatusBadRequest},
	}
	for _, c := range cases {
		status, body := e.do(http.MethodPost, "/v1/host", c.body)
		assert.Equal(t, c.want, status, "%s: %s", c.name, body)
	}
	status, body := e.do(http.MethodPost, "/v1/host", e.hostRequest(op, "host-1", newKey(t), future))
	assert.Equal(t, http.StatusOK, status, "the control: an operator's statement is accepted: %s", body)
}

func TestHost_AnOrchIdentityAloneCannotObtainAHostCertificate(t *testing.T) {
	e := newEnv(t)
	key := newKey(t)
	body := e.signed(e.rootID, identity.ActionIssueOrch, identity.IssueRequest{Name: "orch", CSR: csrFor(t, key)})
	status, out := e.do(http.MethodPost, "/v1/orch", body)
	require.Equal(t, http.StatusOK, status, string(out))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(out, &issued))
	orchID := person{key: key, cert: parseCert(t, issued.Certificate)}
	assert.Equal(t, identity.RoleOrch, roleOf(t, orchID.cert))

	status, out = e.do(http.MethodPost, "/v1/host", e.hostRequest(orchID, "host-1", newKey(t), time.Now().Add(time.Hour)))
	assert.Equal(t, http.StatusForbidden, status, string(out))
	status, out = e.do(http.MethodPost, "/v1/operator", e.signed(orchID, identity.ActionIssueOperator, identity.IssueRequest{Name: "x", Role: identity.RoleOperator, CSR: csrFor(t, newKey(t))}))
	assert.Equal(t, http.StatusForbidden, status, string(out))
}

func TestOperator_OnlyRootIssuesOneAndOnlyOfTheRolesAnOperatorHas(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	reader := e.issue(identity.RoleReader, "bob")
	auto := e.issue(identity.RoleAutomation, "ci")
	for _, p := range []person{op, reader, auto} {
		assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, p.cert.ExtKeyUsage, p.cert.Subject.CommonName)
		assert.WithinDuration(t, time.Now().AddDate(1, 0, 0), p.cert.NotAfter, time.Minute)
		_, err := p.cert.Verify(x509.VerifyOptions{Roots: poolOf(t, e.authorities().Operator), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		assert.NoError(t, err)
	}
	assert.Equal(t, identity.RoleReader, roleOf(t, reader.cert))

	issue := func(by person, role identity.Role) int {
		status, _ := e.do(http.MethodPost, "/v1/operator", e.signed(by, identity.ActionIssueOperator,
			identity.IssueRequest{Name: "carol", Role: role, CSR: csrFor(t, newKey(t))}))
		return status
	}
	assert.Equal(t, http.StatusForbidden, issue(op, identity.RoleOperator), "an operator cannot make operators")
	assert.Equal(t, http.StatusForbidden, issue(reader, identity.RoleReader))
	assert.Equal(t, http.StatusBadRequest, issue(e.rootID, identity.RoleRoot), "root is not issuable through the API")
	assert.Equal(t, http.StatusBadRequest, issue(e.rootID, identity.RoleHost))
	assert.Equal(t, http.StatusOK, issue(e.rootID, identity.RoleReader))

	signed := e.signed(e.rootID, identity.ActionRevoke, identity.RevokeRequest{Serial: "1"})
	status, _ := e.do(http.MethodPost, "/v1/operator", signed)
	assert.Equal(t, http.StatusForbidden, status, "a request signed for another action is refused")
}

func TestRevoke_OperatorsAndRootMayAndTheListIsSignedAndKept(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	reader := e.issue(identity.RoleReader, "bob")
	serial := reader.cert.SerialNumber.Text(16)

	status, _ := e.revoke(reader, serial)
	assert.Equal(t, http.StatusForbidden, status, "a reader may not revoke")
	status, _ = e.revoke(op, "zz")
	assert.Equal(t, http.StatusBadRequest, status)
	status, body := e.revoke(op, strings.ToUpper(serial))
	require.Equal(t, http.StatusOK, status, string(body))
	status, _ = e.revoke(e.rootID, serial)
	require.Equal(t, http.StatusOK, status, "revoking twice changes nothing")

	pub := ed25519.PublicKey(e.authorities().RevocationKey)
	check := func(s *signer.Signer) {
		crlBytes, err := s.CRL()
		require.NoError(t, err)
		crl, err := identity.VerifyCRL(pub, crlBytes)
		require.NoError(t, err)
		assert.Equal(t, []string{serial}, crl.Serials)
		assert.Equal(t, uint64(2), uint64(crl.Seq), "one revocation after the initial list")
	}
	check(e.s)
	status, got := e.do(http.MethodGet, "/v1/crl", nil)
	require.Equal(t, http.StatusOK, status)
	_, err := identity.VerifyCRL(pub, got)
	require.NoError(t, err)
	check(openAt(t, e.dir))

	status, _ = e.do(http.MethodPost, "/v1/host", e.hostRequest(reader, "h", newKey(t), time.Now().Add(time.Hour)))
	assert.Equal(t, http.StatusForbidden, status)
	status, _ = e.do(http.MethodPost, "/v1/revoke", e.signed(reader, identity.ActionRevoke, identity.RevokeRequest{Serial: "1"}))
	assert.Equal(t, http.StatusForbidden, status, "a revoked reader's requests are refused whatever they say")
}

func TestRequest_ARevokedOperatorCannotAdmitAHost(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	status, _ := e.do(http.MethodPost, "/v1/host", e.hostRequest(op, "host-1", newKey(t), time.Now().Add(time.Hour)))
	require.Equal(t, http.StatusOK, status, "before the revocation")

	status, _ = e.revoke(e.rootID, op.cert.SerialNumber.Text(16))
	require.Equal(t, http.StatusOK, status)
	status, body := e.do(http.MethodPost, "/v1/host", e.hostRequest(op, "host-2", newKey(t), time.Now().Add(time.Hour)))
	assert.Equal(t, http.StatusForbidden, status, string(body))
}

func TestBootstrap_LeavesNoUnencryptedPrivateKeyOutsideTheSignersDirectory(t *testing.T) {
	e := newEnv(t)
	person := e.issue(identity.RoleOperator, "alice")
	personFile, err := identity.EncryptPerson("pw", identity.Person{Key: person.key, Cert: person.cert})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(e.root, "alice.person"), personFile, 0o600))
	e.hostCert("host-1")
	status, bundle := e.do(http.MethodGet, "/v1/bundle", nil, "X-Naru-Request",
		base64.StdEncoding.EncodeToString(e.signed(e.rootID, identity.ActionBundle, struct{}{})))
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, os.WriteFile(filepath.Join(e.root, "bundle.sealed"), bundle, 0o600))

	var inside [][]byte
	require.NoError(t, filepath.WalkDir(e.dir, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() || !strings.HasSuffix(path, ".key") {
			return nil
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		block, _ := pem.Decode(data)
		require.NotNil(t, block, path)
		inside = append(inside, block.Bytes)
		return nil
	}))
	require.Len(t, inside, 4, "the control: the signer's directory does hold the keys, or the scan below proves nothing")

	var scanned int
	require.NoError(t, filepath.WalkDir(e.root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			if path == e.dir {
				return fs.SkipDir
			}
			return nil
		}
		scanned++
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "PRIVATE KEY", path)
		for _, key := range inside {
			assert.False(t, bytes.Contains(data, key), "%s holds an authority key", path)
			assert.NotContains(t, string(data), base64.StdEncoding.EncodeToString(key), path)
		}
		return nil
	}))
	assert.GreaterOrEqual(t, scanned, 4, "the recovery key, its passphrase, the person file and the bundle were scanned")
}

func TestRestore_ABundleAndTheRecoveryKeyBringBackTheAuthoritiesTheOldCertificatesChainTo(t *testing.T) {
	e := newEnv(t)
	old := e.hostCert("host-1")
	oldAuth := e.authorities()

	status, bundle := e.do(http.MethodGet, "/v1/bundle", nil, "X-Naru-Request",
		base64.StdEncoding.EncodeToString(e.signed(e.rootID, identity.ActionBundle, struct{}{})))
	require.Equal(t, http.StatusOK, status, string(bundle))
	sum := sha256.Sum256(bundle)
	authz := e.signed(e.rootID, identity.ActionRestore, identity.RestoreRequest{BundleSHA256: hex.EncodeToString(sum[:])})

	e.srv.Close()
	require.NoError(t, os.RemoveAll(e.dir))
	fresh := privateDir(t, t.TempDir())
	s := openAt(t, fresh)
	require.False(t, s.Initialised())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e.srv, e.s, e.dir = srv, s, fresh

	restore := func(b identity.RestoreBody) int {
		raw, err := json.Marshal(b)
		require.NoError(t, err)
		status, _ := e.do(http.MethodPost, "/v1/restore", raw)
		return status
	}
	body := identity.RestoreBody{Bundle: bundle, Authorisation: authz, RecoveryKey: e.recoveryFile, PassphraseFile: e.passFile}

	wrong := filepath.Join(t.TempDir(), "wrong")
	require.NoError(t, os.WriteFile(wrong, []byte("not the passphrase\n"), 0o600))
	assert.Equal(t, http.StatusForbidden, restore(identity.RestoreBody{Bundle: bundle, Authorisation: authz, RecoveryKey: e.recoveryFile, PassphraseFile: wrong}),
		"the wrong passphrase does not open the recovery key")
	other := append([]byte{}, bundle...)
	other[len(other)-1] ^= 1
	assert.Equal(t, http.StatusForbidden, restore(identity.RestoreBody{Bundle: other, Authorisation: authz, RecoveryKey: e.recoveryFile, PassphraseFile: e.passFile}),
		"a bundle that was changed does not open")
	assert.False(t, s.Initialised(), "refusals leave the directory blank")

	require.Equal(t, http.StatusOK, restore(body))
	assert.Equal(t, http.StatusConflict, restore(body), "a directory that holds the authorities is not restored over")

	restored := e.authorities()
	assert.Equal(t, oldAuth, restored)
	_, err := old.Verify(x509.VerifyOptions{Roots: poolOf(t, restored.Host), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	require.NoError(t, err, "the certificate issued before the loss verifies against the restored authority")

	fresh2 := e.hostCert("host-2")
	_, err = fresh2.Verify(x509.VerifyOptions{Roots: poolOf(t, restored.Host), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	require.NoError(t, err, "and a new one issues")
	assert.True(t, openAt(t, fresh).Initialised(), "the directory holds the authorities, so a restart finds them")
}

func TestReplicate_ANewSignerReceivesTheAuthoritiesFromARunningOne(t *testing.T) {
	e := newEnv(t)
	conf, err := e.s.PeerTLSConfig([]string{"127.0.0.1"})
	require.NoError(t, err)
	peer := httptest.NewUnstartedServer(e.s.PeerHandler())
	peer.TLS = conf
	peer.StartTLS()
	defer peer.Close()
	peerCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.authorities().Orch})

	join := func(dir string, authorise func(pub []byte) []byte, ca []byte) (*signer.Signer, error) {
		s := openAt(t, dir)
		pub, err := s.NewJoinKey()
		require.NoError(t, err)
		return s, s.Join(context.Background(), signer.JoinOptions{HostID: "signer-b", PeerURL: peer.URL, PeerCA: ca, Authorisation: authorise(pub)})
	}
	byRoot := func(pub []byte) []byte {
		return e.signed(e.rootID, identity.ActionReplicate, identity.ReplicateRequest{HostID: "signer-b", X25519: pub})
	}

	op := e.issue(identity.RoleOperator, "alice")
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the test reaches routes, not trust
	someKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	byOperator := e.signed(op, identity.ActionReplicate, identity.ReplicateRequest{HostID: "signer-b", X25519: someKey.PublicKey().Bytes()})
	resp, err := insecure.Post(peer.URL+"/v1/replicate", "application/json", bytes.NewReader(byOperator))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the peer itself refuses an operator's request")

	_, err = join(privateDir(t, t.TempDir()), func(pub []byte) []byte {
		return e.signed(op, identity.ActionReplicate, identity.ReplicateRequest{HostID: "signer-b", X25519: pub})
	}, peerCA)
	require.Error(t, err, "an operator may not authorise a replica")

	_, err = join(privateDir(t, t.TempDir()), func([]byte) []byte {
		other, err := identity.GenerateRecovery()
		require.NoError(t, err)
		return e.signed(e.rootID, identity.ActionReplicate, identity.ReplicateRequest{HostID: "signer-b", X25519: other.Public().X25519})
	}, peerCA)
	require.Error(t, err, "an authorisation for another key leaves the answer sealed to a key this signer does not hold")

	wrongCA := newEnv(t).authorities().Orch
	_, err = join(privateDir(t, t.TempDir()), byRoot, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: wrongCA}))
	require.Error(t, err, "a peer that is not the orch authority's is not trusted")

	dir := privateDir(t, t.TempDir())
	b, err := join(dir, byRoot, peerCA)
	require.NoError(t, err)
	assert.True(t, b.Initialised())
	got, err := b.Authorities()
	require.NoError(t, err)
	assert.Equal(t, e.authorities(), got)
	_, statErr := os.Stat(filepath.Join(dir, "join.key"))
	assert.True(t, os.IsNotExist(statErr), "the join key is gone once the authorities are in")

	reopened := openAt(t, dir)
	hostReq := e.hostRequest(op, "host-9", newKey(t), time.Now().Add(time.Hour))
	var req identity.HostRequest
	require.NoError(t, json.Unmarshal(hostReq, &req))
	issued, err := reopened.IssueHost(req)
	require.NoError(t, err, "the replica issues for statements the first signer's operators signed")
	_, err = parseCert(t, issued.Certificate).Verify(x509.VerifyOptions{Roots: poolOf(t, e.authorities().Host), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	require.NoError(t, err)

	resp, err = insecure.Post(peer.URL+"/v1/host", "application/json", bytes.NewReader(hostReq))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "the peer listener serves replication and nothing else")
}

func TestListenSocket_SetsTheModeAndReplacesAStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := signer.ListenSocket(path, 0o660, quiet)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o660), info.Mode().Perm())
	require.NoError(t, l.Close())

	l2, err := signer.ListenSocket(path, 0o600, quiet)
	require.NoError(t, err, "the file a crashed signer left is replaced")
	defer func() { _ = l2.Close() }()
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	plain := filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.WriteFile(plain, nil, 0o600))
	_, err = signer.ListenSocket(plain, 0o600, quiet)
	require.Error(t, err, "a file that is not a socket is not removed")
}

func TestOrch_ItsCertificateCarriesTheNamesItAskedFor(t *testing.T) {
	e := newEnv(t)
	key := newKey(t)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: "orch"},
		DNSNames:    []string{"orch.example"},
		IPAddresses: []net.IP{net.ParseIP("10.0.0.7")},
	}, key)
	require.NoError(t, err)
	status, out := e.do(http.MethodPost, "/v1/orch", e.signed(e.rootID, identity.ActionIssueOrch, identity.IssueRequest{Name: "orch", CSR: der}))
	require.Equal(t, http.StatusOK, status, string(out))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(out, &issued))
	cert := parseCert(t, issued.Certificate)
	assert.Equal(t, []string{"orch.example"}, cert.DNSNames)
	require.Len(t, cert.IPAddresses, 1)
	assert.True(t, cert.IPAddresses[0].Equal(net.ParseIP("10.0.0.7")))
}
