package command_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/naructl/internal/client"
)

// fakeSigner serves one unix socket the way naru-signer does and records what it was asked.
type fakeSigner struct {
	socket string
	roots  *x509.CertPool
	got    map[string]json.RawMessage
	who    map[string]identity.Identity
	status int
	// onCall, when set, is told each path the fake answers.
	onCall func(path string)
	// authorities is what GET /v1/ca answers, which needs no signature.
	authorities identity.Authorities
}

func startFakeSigner(t *testing.T, roots *x509.CertPool) *fakeSigner {
	t.Helper()
	f := &fakeSigner{socket: shortSocketPath(t), roots: roots, got: map[string]json.RawMessage{}, who: map[string]identity.Identity{}, status: http.StatusOK}
	l, err := net.Listen("unix", f.socket)
	require.NoError(t, err)
	actions := map[string]string{"/v1/operator": identity.ActionIssueOperator, "/v1/exposer": identity.ActionIssueExposer, "/v1/revoke": identity.ActionRevoke,
		"/v1/reissue-host": identity.ActionReissueHost, "/v1/register-host": identity.ActionRegisterHost, "/v1/retire-host": identity.ActionRetireHost}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/ca" {
			_ = json.NewEncoder(w).Encode(f.authorities)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		who, body, err := identity.VerifyRequest(raw, actions[r.URL.Path], f.roots, &identity.CRL{}, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		f.got[r.URL.Path], f.who[r.URL.Path] = body, who
		if f.onCall != nil {
			f.onCall(r.URL.Path)
		}
		if f.status != http.StatusOK {
			http.Error(w, "refused by the fake", f.status)
			return
		}
		if r.URL.Path == "/v1/revoke" || r.URL.Path == "/v1/register-host" || r.URL.Path == "/v1/retire-host" {
			_, _ = w.Write([]byte("crl"))
			return
		}
		if r.URL.Path == "/v1/reissue-host" {
			var rot identity.ReissueHostRequest
			_ = json.Unmarshal(body, &rot)
			csr, _ := x509.ParseCertificateRequest(rot.CSR)
			cert := issueFrom(t, caOf(t), rot.HostID, identity.RoleHost, csr.PublicKey)
			_ = json.NewEncoder(w).Encode(identity.Issued{Certificate: cert.Raw, Serial: cert.SerialNumber.Text(16), NotAfter: cert.NotAfter})
			return
		}
		if r.URL.Path == "/v1/exposer" {
			var ex identity.ExposerRequest
			_ = json.Unmarshal(body, &ex)
			subject, _ := identity.ExposerSubject(ex.Network)
			csr, _ := x509.ParseCertificateRequest(ex.CSR)
			cert := issueFrom(t, caOf(t), subject, identity.RoleExposer, csr.PublicKey)
			_ = json.NewEncoder(w).Encode(identity.Issued{Certificate: cert.Raw, Serial: cert.SerialNumber.Text(16), NotAfter: cert.NotAfter})
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

// fakeHost is a host API that takes PUT /v1/identity: it records the pair it was given, and answers
// the serial of that certificate unless status says otherwise.
type fakeHost struct {
	srv    *httptest.Server
	file   string
	status int
	got    map[string]string
	order  *[]string

	mu        sync.Mutex
	presented *tls.Certificate
}

// serve makes the host present a certificate of the serial from the next handshake on, as a host
// that took the pair does. The test server's own certificate is the CA that signs it.
func (h *fakeHost) serve(t *testing.T, serial *big.Int) {
	t.Helper()
	ca := h.srv.TLS.Certificates[0]
	parent, err := x509.ParseCertificate(ca.Certificate[0])
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "h-1"}, DNSNames: []string{"example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, parent, &key.PublicKey, ca.PrivateKey)
	require.NoError(t, err)
	h.mu.Lock()
	h.presented = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	h.mu.Unlock()
}

func startFakeHost(t *testing.T, order *[]string) *fakeHost {
	t.Helper()
	h := &fakeHost{status: http.StatusOK, order: order}
	h.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*h.order = append(*h.order, r.Method+" "+r.URL.Path)
		if h.status != http.StatusOK {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(`{"error":"the host refused"}`))
			return
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&h.got))
		b, _ := pem.Decode([]byte(h.got["certificate"]))
		cert, err := x509.ParseCertificate(b.Bytes)
		require.NoError(t, err)
		h.serve(t, cert.SerialNumber)
		_ = json.NewEncoder(w).Encode(map[string]string{"serial": cert.SerialNumber.Text(16)})
	}))
	h.srv.TLS = &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.presented != nil {
			return h.presented, nil
		}
		return &h.srv.TLS.Certificates[0], nil
	}}
	h.srv.StartTLS()
	t.Cleanup(h.srv.Close)
	pair := caOf(t)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.srv.Certificate().Raw})
	keyDER, err := x509.MarshalPKCS8PrivateKey(pair.key)
	require.NoError(t, err)
	f := client.File{
		Endpoints: []string{h.srv.Listener.Addr().String()}, CA: string(ca),
		Certificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pair.cert.Raw})),
		Key:         string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}
	h.file = filepath.Join(t.TempDir(), "host.yaml")
	require.NoError(t, f.Write(h.file))
	return h
}

func TestSignerRotateHost_DeliversThePairBeforeTheSignerRevokesAnything(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	var order []string
	f.onCall = func(path string) { order = append(order, "signer "+path) }
	h := startFakeHost(t, &order)
	admin, pass := adminFile(t, identity.RoleOperator, "pw")

	text, err := run(t, "signer", "rotate-host", "--admin", admin, "--passphrase-file", pass, "--host-id", "h-1", "--host-client", h.file, "--host-server-name", "example.com", "--socket", f.socket)
	require.NoError(t, err, text)
	assert.Equal(t, []string{"signer /v1/reissue-host", "PUT /v1/identity", "signer /v1/retire-host"}, order, "the host holds the new pair before the old serials are revoked")
	var req identity.ReissueHostRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/reissue-host"], &req))
	assert.Equal(t, "h-1", req.HostID)
	assert.Equal(t, identity.RoleOperator, f.who["/v1/reissue-host"].Role)

	kb, _ := pem.Decode([]byte(h.got["key"]))
	require.NotNil(t, kb)
	key, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	require.NoError(t, err)
	cb, _ := pem.Decode([]byte(h.got["certificate"]))
	require.NotNil(t, cb)
	cert, err := x509.ParseCertificate(cb.Bytes)
	require.NoError(t, err)
	assert.True(t, key.(*ecdsa.PrivateKey).PublicKey.Equal(cert.PublicKey), "the certificate sent is for the key sent with it")
	var retire identity.RetireHostRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/retire-host"], &retire))
	assert.Equal(t, cert.SerialNumber.Text(16), retire.Keep, "the serial that is kept is the delivered one")
	assert.Contains(t, text, "old serials are revoked")
}

func TestSignerRotateHost_RevokesNothingWhenTheSignerOrTheHostRefuses(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	var order []string
	h := startFakeHost(t, &order)
	admin, pass := adminFile(t, identity.RoleOperator, "pw")
	args := []string{"signer", "rotate-host", "--admin", admin, "--passphrase-file", pass, "--host-id", "h-1", "--host-client", h.file, "--host-server-name", "example.com", "--socket", f.socket}

	f.status = http.StatusConflict
	_, err := run(t, args...)
	require.Error(t, err)
	assert.Empty(t, order, "a refused issue sends nothing to the host")
	assert.NotContains(t, f.got, "/v1/retire-host")

	f.status = http.StatusOK
	h.status = http.StatusBadGateway
	_, err = run(t, args...)
	require.ErrorContains(t, err, "old pair is still valid and in use")
	assert.NotContains(t, f.got, "/v1/retire-host", "a host that did not take the pair leaves the old serials unrevoked")
}

func TestSignerRegisterHost_SendsTheSerialOfTheCertificateAndRefusesAnotherHostsOne(t *testing.T) {
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t).cert)
	f := startFakeSigner(t, pool)
	admin, pass := adminFile(t, identity.RoleOperator, "pw")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	cert := issueFrom(t, caOf(t), "h-1", identity.RoleHost, &key.PublicKey)
	file := filepath.Join(t.TempDir(), "host.crt")
	require.NoError(t, os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644))

	_, err = run(t, "signer", "register-host", "--admin", admin, "--passphrase-file", pass, "--host-id", "h-2", "--cert", file, "--socket", f.socket)
	require.Error(t, err, "the certificate is h-1's")
	assert.NotContains(t, f.got, "/v1/register-host")

	text, err := run(t, "signer", "register-host", "--admin", admin, "--passphrase-file", pass, "--host-id", "h-1", "--cert", file, "--socket", f.socket)
	require.NoError(t, err, text)
	var req identity.RegisterHostRequest
	require.NoError(t, json.Unmarshal(f.got["/v1/register-host"], &req))
	assert.Equal(t, identity.RegisterHostRequest{HostID: "h-1", Serial: cert.SerialNumber.Text(16)}, req)
}
