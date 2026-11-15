package api_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/init/api"
	"github.com/garamsh/naru/lib/init/config"
)

// seen is what a fake signer received of one request.
type seen struct {
	method, path, host, query string
	header                    http.Header
	body                      []byte
}

// fakeSigner serves handler on a unix socket and records each request it receives.
type fakeSigner struct {
	socket string
	mu     sync.Mutex
	got    []seen
}

func (f *fakeSigner) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.got...)
}

// serveSigner serves handler on a unix socket in a short directory: a socket path may be 107 bytes.
func serveSigner(t *testing.T, handler http.Handler) *fakeSigner {
	t.Helper()
	dir, err := os.MkdirTemp("", "sg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &fakeSigner{socket: dir + "/s.sock"}
	l, err := net.Listen("unix", f.socket)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.got = append(f.got, seen{r.Method, r.URL.Path, r.Host, r.URL.RawQuery, r.Header.Clone(), body})
		f.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		handler.ServeHTTP(w, r)
	}))
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return f
}

// signerRig is a Node API whose configuration runs a signer, forwarding to socket.
func signerRig(t *testing.T, socket string) *rig {
	t.Helper()
	r := newRig(t)
	r.chosen.Config.Spec.Signer = &config.SignerSection{}
	r.engine.cfg = r.chosen.Config
	r.server = api.New(api.Deps{
		Version:      "v1.2.3",
		BootID:       func() (string, error) { return "boot-1", nil },
		Uptime:       func() time.Duration { return time.Second },
		SignerSocket: socket,
		Log:          slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	t.Cleanup(func() { _ = r.server.Close() })
	r.start(t)
	return r
}

func answer(status int, contentType, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func TestSigner_AnAdminReachesTheSignerWithMethodBodyAndHeaderAndGetsItsAnswerBack(t *testing.T) {
	f := serveSigner(t, answer(http.StatusTeapot, "application/octet-stream", "sealed-bytes"))
	r := signerRig(t, f.socket)

	req, err := http.NewRequest(http.MethodPost, r.url("/v1/signer/restore?x=1"), strings.NewReader(`{"bundle":"AA"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Naru-Request", "c2lnbmVk")
	req.Header.Set("Authorization", "Bearer must-not-pass")
	req.Header.Set("Cookie", "must-not-pass=1")
	resp, err := r.admin(t).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(t, http.StatusTeapot, resp.StatusCode, "the signer's status comes back unchanged")
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "sealed-bytes", string(body))
	got := f.requests()
	require.Len(t, got, 1)
	assert.Equal(t, http.MethodPost, got[0].method)
	assert.Equal(t, "/v1/restore", got[0].path, "the signer sees its own path")
	assert.Equal(t, "naru-signer", got[0].host)
	assert.Equal(t, `{"bundle":"AA"}`, string(got[0].body))
	assert.Equal(t, "application/json", got[0].header.Get("Content-Type"))
	assert.Equal(t, "c2lnbmVk", got[0].header.Get("X-Naru-Request"), "the signed request of a GET travels in this header")
	for _, name := range []string{"Authorization", "Cookie", "X-Forwarded-For", "Forwarded", "X-Forwarded-Host"} {
		assert.Empty(t, got[0].header.Values(name), "%s is not forwarded: the proxy adds no header the signer could trust", name)
	}
	assert.NotContains(t, r.logs.String(), "sealed-bytes")
	assert.NotContains(t, r.logs.String(), `"bundle"`, "a body is never logged")
}

func TestSigner_EveryMethodIsForwardedToAnAdminOnly(t *testing.T) {
	f := serveSigner(t, answer(http.StatusOK, "application/json", `{}`))
	r := signerRig(t, f.socket)
	reader := r.client(t, r.clientCA, "naru:reader")

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		status, _ := r.do(t, reader, method, "/v1/signer/ca", "")
		assert.Equal(t, http.StatusForbidden, status, "a reader may not %s the signer", method)
		status, _ = r.do(t, r.admin(t), method, "/v1/signer/ca", "")
		assert.Equal(t, http.StatusOK, status, "the control: an admin may %s it", method)
	}
	status, _ := r.do(t, reader, http.MethodGet, "/v1/signer", "")
	assert.Equal(t, http.StatusForbidden, status, "the route itself needs Admin, not only what is under it")
	status, _ = r.do(t, reader, http.MethodGet, "/v1/version", "")
	assert.Equal(t, http.StatusOK, status, "the control: a reader still reads the rest of the API")
	assert.Len(t, f.requests(), 4, "only the admin's four requests reached the signer")
}

func TestSigner_ANodeWithoutASignerAnswers404(t *testing.T) {
	f := serveSigner(t, answer(http.StatusOK, "application/json", `{}`))
	r := signerRig(t, f.socket)
	status, _ := r.do(t, r.admin(t), http.MethodGet, "/v1/signer/ca", "")
	require.Equal(t, http.StatusOK, status, "the control: with spec.signer the request is forwarded")

	r.engine.mu.Lock()
	r.engine.cfg.Spec.Signer = nil
	r.engine.mu.Unlock()
	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/signer/ca", "")

	assert.Equal(t, http.StatusNotFound, status)
	assert.Contains(t, decode(t, body)["error"], "no signer")
	assert.Len(t, f.requests(), 1, "the refused request did not reach the socket")
}

func TestSigner_ASocketThatCannotBeDialledAnswers503(t *testing.T) {
	f := serveSigner(t, answer(http.StatusOK, "application/json", `{}`))
	live := signerRig(t, f.socket)
	status, _ := live.do(t, live.admin(t), http.MethodGet, "/v1/signer/ca", "")
	require.Equal(t, http.StatusOK, status, "the control: a socket that is served is reached")

	dead := signerRig(t, f.socket+".gone")
	status, body := dead.do(t, dead.admin(t), http.MethodGet, "/v1/signer/ca", "")

	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Contains(t, decode(t, body)["error"], "cannot be reached")
}

func TestSigner_ABodyLargerThanTheSignerReadsIsRefusedAndOneAtTheBoundIsNot(t *testing.T) {
	f := serveSigner(t, answer(http.StatusOK, "application/json", `{}`))
	r := signerRig(t, f.socket)
	const bound = 1 << 20

	status, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/signer/restore", strings.Repeat("a", bound))
	assert.Equal(t, http.StatusOK, status, "a body of the bound is forwarded")
	status, _ = r.do(t, r.admin(t), http.MethodPost, "/v1/signer/restore", strings.Repeat("a", bound+1))
	assert.Equal(t, http.StatusRequestEntityTooLarge, status, "a known length over the bound")

	req, err := http.NewRequest(http.MethodPost, r.url("/v1/signer/restore"), io.NopCloser(strings.NewReader(strings.Repeat("a", bound+1))))
	require.NoError(t, err)
	resp, err := r.admin(t).Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, "a body of no known length is cut at the bound")
	// The proxy streams, so the signer may have begun to receive the cut body before it was refused;
	// what must not happen is a body over the bound reaching it whole.
	for _, got := range f.requests() {
		assert.LessOrEqual(t, len(got.body), bound, "no oversize body was delivered whole")
	}
	assert.NotEmpty(t, f.requests(), "the control: the body of the bound was forwarded")
}

func TestSigner_APathThatIsNotCleanIsRefused(t *testing.T) {
	f := serveSigner(t, answer(http.StatusOK, "application/json", `{}`))
	r := signerRig(t, f.socket)

	status, _ := r.do(t, r.admin(t), http.MethodGet, "/v1/signer/a%2F..%2Fca", "")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = r.do(t, r.admin(t), http.MethodGet, "/v1/signer/ca", "")
	assert.Equal(t, http.StatusOK, status, "the control: a clean path is forwarded")
}

// operatorSigner is a fake of the signer's POST /v1/operator that authenticates as the signer does:
// by the signature on the request, with identity.VerifyRequest against its roots.
func operatorSigner(roots *x509.CertPool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if _, _, err := identity.VerifyRequest(body, identity.ActionIssueOperator, roots, nil, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "issued")
	})
}

// operator returns a root of its own and a certificate and key that root signed for the role.
func operator(t *testing.T) (roots *x509.CertPool, cert *x509.Certificate, key *ecdsa.PrivateKey) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rootTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	require.NoError(t, err)
	root, err := x509.ParseCertificate(rootDER)
	require.NoError(t, err)
	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "alice", OrganizationalUnit: []string{string(identity.RoleOperator)}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root, &key.PublicKey, rootKey)
	require.NoError(t, err)
	cert, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	roots = x509.NewCertPool()
	roots.AddCert(root)
	return roots, cert, key
}

func TestSigner_ARequestWithoutAValidSignatureIsRefusedByTheSignerAndASignedOneIsNot(t *testing.T) {
	roots, cert, key := operator(t)
	f := serveSigner(t, operatorSigner(roots))
	r := signerRig(t, f.socket)
	signed, err := identity.SignRequest(identity.ActionIssueOperator, map[string]string{"name": "bob"}, time.Now().Add(time.Minute), key, cert)
	require.NoError(t, err)
	tampered := []byte(strings.Replace(string(signed), `"signature":"`, `"signature":"AAAA`, 1))
	require.NotEqual(t, string(signed), string(tampered), "the setup changed the signature")

	status, body := r.do(t, r.admin(t), http.MethodPost, "/v1/signer/operator", string(signed))
	assert.Equal(t, http.StatusOK, status, "the control: the same request, signed, is accepted through the Node API")
	assert.Equal(t, "issued", string(body))

	status, _ = r.do(t, r.admin(t), http.MethodPost, "/v1/signer/operator", `{"name":"bob"}`)
	assert.Equal(t, http.StatusForbidden, status, "an admin of the Node API holds no authority over the signer: an unsigned request is refused")
	status, _ = r.do(t, r.admin(t), http.MethodPost, "/v1/signer/operator", "")
	assert.Equal(t, http.StatusForbidden, status, "an empty body is refused")
	status, _ = r.do(t, r.admin(t), http.MethodPost, "/v1/signer/operator", string(tampered))
	assert.Equal(t, http.StatusForbidden, status, "a request whose signature is damaged is refused")
}
