package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/hostapi"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// authority is a throwaway CA.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newAuthority(t *testing.T, cn string) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return authority{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a certificate signed by the authority, as a tls pair.
func (a authority) issue(t *testing.T, orgs []string, server bool) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "t", Organization: orgs},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, a.cert, &key.PublicKey, a.key)
	require.NoError(t, err)
	kder, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return pair, certPEM, keyPEM
}

type env struct {
	rt       *fakeRuntime
	addr     string
	ca       authority
	hostFile string
	pool     *x509.CertPool
	metrics  *prometheus.Registry
	nets     *network.Manager
	nft      *recorder
}

// recorder is the network.Runner of the tests: it records what the host would have run.
type recorder struct {
	mu    sync.Mutex
	calls [][]string
	stdin []string
}

func (r *recorder) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.stdin = append(r.stdin, string(stdin))
	return nil, nil
}

// ruleset is the last nftables script the host applied.
func (r *recorder) ruleset() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i][0] == "nft" {
			return r.stdin[i]
		}
	}
	return ""
}

func start(t *testing.T) *env { return startWith(t, netip.MustParseAddr("10.1.0.1")) }

// startWith starts a server whose machine gateway is gateway, with networks.
func startWith(t *testing.T, gateway netip.Addr) *env { return startEnv(t, gateway, true) }

// startEnv starts a server whose machine gateway is gateway; it has networks when networks is true.
func startEnv(t *testing.T, gateway netip.Addr, networks bool) *env {
	t.Helper()
	dir := t.TempDir()
	ca := newAuthority(t, "clients")
	srvCA := newAuthority(t, "servers")
	_, crt, key := srvCA.issue(t, nil, true)
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, b, 0o600))
		return p
	}
	conf, err := TLSConfig(write("tls.crt", crt), write("tls.key", key), write("ca.crt", ca.pem))
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e := &env{rt: newFake(), addr: ln.Addr().String(), ca: ca, pool: x509.NewCertPool()}
	e.pool.AppendCertsFromPEM(srvCA.pem)
	e.hostFile = write("host.json", []byte(`{"region":"eu-1","labels":{"disk":"ssd"}}`))
	e.metrics = prometheus.NewRegistry()
	e.nft = &recorder{}
	e.nets = &network.Manager{
		Run: e.nft, KeyFile: "/run/naru-hostd/wg.key", Port: 51820,
		Base: network.Config{
			Management: netip.MustParsePrefix("172.16.0.0/24"), HostAddrs: []netip.Addr{netip.MustParseAddr("192.0.2.1")},
			TapPrefix: "tap", Egress: "eth0", Tunnel: "wg-naru",
		},
	}
	deps := Deps{Runtime: e.rt, HostFile: e.hostFile, MachineGateway: gateway, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: e.metrics}
	if networks {
		deps.Networks = e.nets
	}
	srv := New(deps)
	go func() { _ = srv.Serve(ln, conf) }()
	t.Cleanup(func() { _ = ln.Close() })
	return e
}

func (e *env) client(pair tls.Certificate) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: e.pool, Certificates: []tls.Certificate{pair},
	}}}
}

func (e *env) as(t *testing.T, role string) *http.Client {
	t.Helper()
	pair, _, _ := e.ca.issue(t, []string{role}, false)
	return e.client(pair)
}

func (e *env) do(t *testing.T, c *http.Client, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://"+e.addr+path, rd)
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func createBody() map[string]any {
	return map[string]any{
		"name": "web", "image": "registry.example/app@" + digest, "cpu": 2, "memory": 1 << 28, "rootDiskBytes": 1 << 30,
		"disks": []map[string]any{{"name": "data", "sizeBytes": 1 << 20}},
	}
}

func TestHost_ReportsTheHostFileOnEveryRequest(t *testing.T) {
	e := start(t)
	c := e.as(t, ReaderOrganization)
	code, b := e.do(t, c, "GET", "/v1/host", nil)
	require.Equal(t, 200, code)
	var h hostapi.Host
	require.NoError(t, json.Unmarshal(b, &h))
	assert.Equal(t, "eu-1", h.Region)
	assert.Equal(t, map[string]string{"disk": "ssd"}, h.Labels)
	assert.Equal(t, uint32(8), h.Capacity.CPU)

	// A changed file shows with no restart.
	require.NoError(t, os.WriteFile(e.hostFile, []byte(`{"region":"eu-1","labels":{"disk":"nvme"}}`), 0o600))
	_, b = e.do(t, c, "GET", "/v1/host", nil)
	require.NoError(t, json.Unmarshal(b, &h))
	assert.Equal(t, "nvme", h.Labels["disk"])
}

func TestRoles_AReaderMayNotWrite(t *testing.T) {
	e := start(t)
	reader := e.as(t, ReaderOrganization)
	code, _ := e.do(t, reader, "POST", "/v1/microvms", createBody())
	assert.Equal(t, 403, code)
	code, _ = e.do(t, reader, "DELETE", "/v1/microvms/web", nil)
	assert.Equal(t, 403, code)
	assert.Empty(t, e.rt.machines, "a refused request reached mvmd")

	// The control: the same body from a provisioner is accepted.
	code, _ = e.do(t, e.as(t, ProvisionerOrganization), "POST", "/v1/microvms", createBody())
	assert.Equal(t, 201, code)
	code, _ = e.do(t, reader, "GET", "/v1/microvms", nil)
	assert.Equal(t, 200, code)
}

func TestRoles_ACertificateWithoutARoleIsRefused(t *testing.T) {
	e := start(t)
	code, _ := e.do(t, e.as(t, "other"), "GET", "/v1/host", nil)
	assert.Equal(t, 403, code)
}

func TestTLS_AClientOfAnotherCAFailsTheHandshake(t *testing.T) {
	e := start(t)
	other := newAuthority(t, "foreign")
	pair, _, _ := other.issue(t, []string{ProvisionerOrganization}, false)
	req, err := http.NewRequest("GET", "https://"+e.addr+"/v1/host", nil)
	require.NoError(t, err)
	_, err = e.client(pair).Do(req)
	assert.Error(t, err)

	// The control: a client of the right CA gets through.
	code, _ := e.do(t, e.as(t, ReaderOrganization), "GET", "/v1/host", nil)
	assert.Equal(t, 200, code)
}

func TestCreate_IsIdempotentAndAnotherBodyConflicts(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, first := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	assert.Equal(t, []string{"registry.example/app@" + digest}, e.rt.pulled)

	code, again := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 200, code)
	assert.JSONEq(t, string(first), string(again))
	assert.Len(t, e.rt.machines, 1)

	other := createBody()
	other["cpu"] = 4
	code, _ = e.do(t, c, "POST", "/v1/microvms", other)
	assert.Equal(t, 409, code)

	bigger := createBody()
	bigger["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 21}}
	code, _ = e.do(t, c, "POST", "/v1/microvms", bigger)
	assert.Equal(t, 409, code, "a disk of another size is another body")
}

func TestCreate_AFailedCreateRemovesTheDisksItMadeAndNotThoseAnotherRequestMade(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	// Another request has made web.data; this one's CreateDisk for it conflicts, after web.a was made.
	e.rt.disks["web.data"] = mvmd.Disk{ID: "web.data", SizeBytes: 1 << 21}
	body := createBody()
	body["disks"] = []map[string]any{{"name": "a", "sizeBytes": 1 << 20}, {"name": "data", "sizeBytes": 1 << 20}}
	code, _ := e.do(t, c, "POST", "/v1/microvms", body)
	assert.Equal(t, 409, code)
	assert.Contains(t, e.rt.disks, "web.data", "the other request's disk was deleted")
	assert.Equal(t, uint64(1<<21), e.rt.disks["web.data"].SizeBytes)
	assert.NotContains(t, e.rt.disks, "web.a", "the control: the disk this request made is removed")
	assert.Empty(t, e.rt.machines)
}

func TestCreate_StartsAMachineAnEarlierAttemptLeftCreated(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	m := e.rt.machines["web"]
	m.State = "created"
	e.rt.machines["web"] = m
	code, b := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 200, code)
	assert.Contains(t, string(b), `"state":"running"`)
}

func TestCreate_Validation(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	cases := map[string]func(map[string]any){
		"name":          func(b map[string]any) { b["name"] = "Bad.Name" },
		"image":         func(b map[string]any) { b["image"] = "latest" },
		"cpu":           func(b map[string]any) { b["cpu"] = 0 },
		"memory":        func(b map[string]any) { b["memory"] = 0 },
		"rootDiskBytes": func(b map[string]any) { b["rootDiskBytes"] = 0 },
		"disks[1]": func(b map[string]any) {
			b["disks"] = []map[string]any{{"name": "a", "sizeBytes": 1}, {"name": "a", "sizeBytes": 1}}
		},
		"networkPolicy.egress[0]": func(b map[string]any) {
			b["networkPolicy"] = map[string]any{"egress": []map[string]any{{"prefix": "10.0.0.1/8", "protocol": "tcp"}}}
		},
		"env": func(b map[string]any) { b["env"] = map[string]string{"A": "b"} },
	}
	for field, mutate := range cases {
		t.Run(field, func(t *testing.T) {
			b := createBody()
			mutate(b)
			code, body := e.do(t, c, "POST", "/v1/microvms", b)
			assert.Equal(t, 422, code)
			assert.Contains(t, string(body), field)
		})
	}
	code, _ := e.do(t, c, "POST", "/v1/microvms", map[string]any{"name": "x", "bogus": 1})
	assert.Equal(t, 400, code, "an unknown field")
	assert.Empty(t, e.rt.machines)
	assert.Empty(t, e.rt.pulled)
}

func TestMicroVMs_GetListDelete(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)

	code, b := e.do(t, c, "GET", "/v1/microvms/web", nil)
	require.Equal(t, 200, code)
	var m hostapi.MicroVM
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, "running", m.State)
	assert.Equal(t, []string{"10.1.0.5"}, m.Addresses)
	assert.Equal(t, []hostapi.DiskSpec{{Name: "data", SizeBytes: 1 << 20}}, m.Disks)

	_, b = e.do(t, c, "GET", "/v1/microvms", nil)
	assert.Contains(t, string(b), `"web"`)

	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	assert.Equal(t, 204, code)
	assert.Empty(t, e.rt.machines)
	assert.Empty(t, e.rt.disks, "the disk made with the microVM went with it")
	code, _ = e.do(t, c, "GET", "/v1/microvms/web", nil)
	assert.Equal(t, 404, code)
	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	assert.Equal(t, 404, code)
}

func TestDelete_RetryRemovesDisksAHalfDoneDeleteLeft(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	e.rt.disks["web.data"] = mvmdDisk("web.data")
	e.rt.disks["webb.data"] = mvmdDisk("webb.data")
	code, _ := e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	assert.Equal(t, 404, code)
	assert.NotContains(t, e.rt.disks, "web.data")
	assert.Contains(t, e.rt.disks, "webb.data", "another microVM's disk is left")
}
