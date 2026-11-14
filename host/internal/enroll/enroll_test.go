package enroll_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/lib/orchapi"
)

// fakeOrch is orch as far as an enrolling host sees it: it keeps the enrolled host pending until
// told to admit it, then issues a certificate for the key it enrolled with.
type fakeOrch struct {
	t       *testing.T
	ca      *x509.Certificate
	caKey   *ecdsa.PrivateKey
	secret  string
	srv     *httptest.Server
	mu      sync.Mutex
	enrolls int
	hearts  int
	pub     []byte
	csr     []byte
	cert    []byte
	hostID  string
	// heartCN is the subject of the last heartbeat's client certificate.
	heartCN string
}

func newFakeOrch(t *testing.T, secret string) *fakeOrch {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	f := &fakeOrch{t: t, ca: ca, caKey: caKey, secret: secret}

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	sTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "orch"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	sDER, err := x509.CreateCertificate(rand.Reader, sTmpl, ca, &srvKey.PublicKey, caKey)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enroll", f.enroll)
	mux.HandleFunc("GET /v1/enroll/{id}", f.poll)
	mux.HandleFunc("POST /v1/heartbeat", f.heartbeat)
	f.srv = httptest.NewUnstartedServer(mux)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{sDER, ca.Raw}, PrivateKey: srvKey}}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOrch) fingerprint() string {
	sum := sha256.Sum256(f.ca.Raw)
	return hex.EncodeToString(sum[:])
}

func (f *fakeOrch) json(w http.ResponseWriter, status int, v any) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeOrch) enroll(w http.ResponseWriter, r *http.Request) {
	var req orchapi.EnrollRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Key != f.secret {
		f.json(w, http.StatusForbidden, orchapi.Error{Message: "enrollment key refused"})
		return
	}
	f.enrolls++
	f.pub, f.csr, f.hostID = req.PublicKey, req.CSR, req.HostID
	f.json(w, http.StatusOK, orchapi.EnrollStatus{State: orchapi.StatePending, Code: "ABCD-EFGH"})
}

func (f *fakeOrch) poll(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := orchapi.VerifyProof(f.pub, f.hostID, r.Header.Get(orchapi.HeaderTime), r.Header.Get(orchapi.HeaderSignature), time.Now()); err != nil {
		f.json(w, http.StatusBadRequest, orchapi.Error{Message: err.Error()})
		return
	}
	if f.cert == nil {
		f.json(w, http.StatusOK, orchapi.EnrollStatus{State: orchapi.StatePending, Code: "ABCD-EFGH"})
		return
	}
	f.json(w, http.StatusOK, orchapi.EnrollStatus{State: orchapi.StateAdmitted, Certificate: f.cert})
}

func (f *fakeOrch) heartbeat(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		f.json(w, http.StatusUnauthorized, orchapi.Error{Message: "a client certificate is required"})
		return
	}
	f.mu.Lock()
	f.hearts++
	f.heartCN = r.TLS.PeerCertificates[0].Subject.CommonName
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// admit issues the certificate for the enrolled key.
func (f *fakeOrch) admit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	csr, err := x509.ParseCertificateRequest(f.csr)
	require.NoError(f.t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: f.hostID}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.ca, csr.PublicKey, f.caKey)
	require.NoError(f.t, err)
	f.cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func (f *fakeOrch) counts() (enrolls, hearts int, cn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enrolls, f.hearts, f.heartCN
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func agent(t *testing.T, f *fakeOrch, dir string, out *lockedBuffer) *enroll.Agent {
	t.Helper()
	keyFile := filepath.Join(dir, "host.key")
	secretFile := filepath.Join(dir, "enroll.key")
	require.NoError(t, os.WriteFile(secretFile, []byte(f.secret+"\n"), 0o600))
	return enroll.New(enroll.Config{
		OrchURL: f.srv.URL, CASHA256: f.fingerprint(), EnrollKeyFile: secretFile, KeyFile: keyFile,
		HostID: "host-a", Address: "10.0.0.5:7443", Out: out,
		CodeEvery: 150 * time.Millisecond, PollEvery: 30 * time.Millisecond, HeartbeatEvery: 30 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
}

func runAgent(t *testing.T, a *enroll.Agent) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the agent did not stop")
		}
	}
}

func TestEnrollShowsTheCodeThenWritesTheCertificateAndHeartbeats(t *testing.T) {
	f := newFakeOrch(t, "naru-ek-secret")
	dir := t.TempDir()
	out := &lockedBuffer{}
	stop := runAgent(t, agent(t, f, dir, out))

	require.Eventually(t, func() bool { return strings.Count(out.String(), "ABCD-EFGH") >= 2 }, 5*time.Second, 20*time.Millisecond, "the code is printed again while pending")
	_, err := os.Stat(filepath.Join(dir, "host.crt"))
	require.ErrorIs(t, err, os.ErrNotExist, "no certificate before admission")

	f.admit()
	require.Eventually(t, func() bool { _, hearts, _ := f.counts(); return hearts >= 2 }, 5*time.Second, 20*time.Millisecond)
	_, _, cn := f.counts()
	require.Equal(t, "host-a", cn, "heartbeats carry the issued certificate")
	stop()

	st, err := os.Stat(filepath.Join(dir, "host.key"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	require.NotContains(t, out.String(), "naru-ek-secret", "the key is never printed")
}

func TestARestartKeepsTheKeyAndTheCertificate(t *testing.T) {
	f := newFakeOrch(t, "naru-ek-secret")
	dir := t.TempDir()
	out := &lockedBuffer{}
	stop := runAgent(t, agent(t, f, dir, out))
	require.Eventually(t, func() bool { e, _, _ := f.counts(); return e == 1 }, 5*time.Second, 20*time.Millisecond)
	f.admit()
	require.Eventually(t, func() bool { _, hearts, _ := f.counts(); return hearts >= 1 }, 5*time.Second, 20*time.Millisecond)
	stop()
	key1, err := os.ReadFile(filepath.Join(dir, "host.key"))
	require.NoError(t, err)
	cert1, err := os.ReadFile(filepath.Join(dir, "host.crt"))
	require.NoError(t, err)

	stop = runAgent(t, agent(t, f, dir, out))
	require.Eventually(t, func() bool { _, hearts, _ := f.counts(); return hearts >= 3 }, 5*time.Second, 20*time.Millisecond)
	stop()

	enrolls, _, _ := f.counts()
	require.Equal(t, 1, enrolls, "the restarted host did not enroll again")
	key2, _ := os.ReadFile(filepath.Join(dir, "host.key"))
	cert2, _ := os.ReadFile(filepath.Join(dir, "host.crt"))
	require.Equal(t, key1, key2)
	require.Equal(t, cert1, cert2)
}

func TestARefusedEnrollmentKeyEndsTheEnrollment(t *testing.T) {
	f := newFakeOrch(t, "naru-ek-secret")
	dir := t.TempDir()
	a := agent(t, f, dir, &lockedBuffer{})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "enroll.key"), []byte("naru-ek-other"), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := a.Run(ctx)
	require.ErrorContains(t, err, "refused")
}
