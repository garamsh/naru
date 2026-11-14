package console_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/naructl/internal/console"
)

var now = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// fakeOrch is an orch that holds what a test puts in it and records the admissions posted.
type fakeOrch struct {
	mu       sync.Mutex
	hosts    []orchapi.Host
	events   []orchapi.Event
	pending  []orchapi.Pending
	admitErr error
	admitted []orchapi.AdmitRequest
	fleet    *fakeFleet
}

func (f *fakeOrch) Hosts(context.Context) ([]orchapi.Host, error)      { return f.hosts, nil }
func (f *fakeOrch) Events(context.Context) ([]orchapi.Event, error)    { return f.events, nil }
func (f *fakeOrch) Pending(context.Context) ([]orchapi.Pending, error) { return f.pending, nil }
func (f *fakeOrch) Admit(_ context.Context, req orchapi.AdmitRequest) (orchapi.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.admitErr != nil {
		return orchapi.Host{}, f.admitErr
	}
	f.admitted = append(f.admitted, req)
	return orchapi.Host{ID: req.HostID, Region: req.Region, Labels: req.Labels}, nil
}

// fakeFleet holds the microVMs and groups.
type fakeFleet struct {
	vms    map[string][]console.MicroVM
	vmErr  map[string]error
	groups []console.Group
}

func (f *fakeFleet) MicroVMs(_ context.Context, h string) ([]console.MicroVM, error) {
	return f.vms[h], f.vmErr[h]
}
func (f *fakeFleet) Groups(context.Context) ([]console.Group, error) { return f.groups, nil }

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return authority{cert, key, pool}
}

func (a authority) person(t *testing.T, role string) identity.Person {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "alice", OrganizationalUnit: []string{role}},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return identity.Person{Key: key, Cert: cert}
}

func hostKeyDER(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return der
}

type env struct {
	orch  *fakeOrch
	ca    authority
	srv   *httptest.Server
	hosts []string
}

func start(t *testing.T, orch *fakeOrch, allowedHosts func(srvHost string) []string) env {
	t.Helper()
	ca := newAuthority(t)
	person := ca.person(t, "operator")
	e := env{orch: orch, ca: ca}
	var s *console.Server
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.Handler().ServeHTTP(w, r) })
	e.srv = httptest.NewServer(h)
	t.Cleanup(e.srv.Close)
	cfg := console.Config{
		Orch:         orch,
		Sign:         func(a identity.Admission) ([]byte, error) { return identity.SignAdmission(a, person.Key, person.Cert) },
		AdmissionTTL: time.Hour,
		Now:          func() time.Time { return now },
	}
	if orch.fleet != nil { // a nil *fakeFleet in the interface would not be nil
		cfg.Fleet = orch.fleet
	}
	if allowedHosts != nil {
		cfg.AllowedHosts = allowedHosts(strings.TrimPrefix(e.srv.URL, "http://"))
	}
	var err error
	s, err = console.New(cfg)
	require.NoError(t, err)
	return e
}

func (e env) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func (e env) post(t *testing.T, path string, form url.Values) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.PostForm(e.srv.URL+path, form)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if resp.StatusCode == http.StatusSeeOther {
		return resp.StatusCode, resp.Header.Get("Location")
	}
	return resp.StatusCode, string(b)
}

var csrfField = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func (e env) csrf(t *testing.T) string {
	t.Helper()
	_, body := e.get(t, "/pending")
	m := csrfField.FindStringSubmatch(body)
	require.NotNil(t, m, "the pending page carries a CSRF token: %s", body)
	return m[1]
}

func sampleOrch() *fakeOrch {
	return &fakeOrch{
		hosts: []orchapi.Host{
			{ID: "host-b", Region: "kr-2", Address: "10.0.0.2", LastSeen: now, Draining: true},
			{ID: "host-a", Region: "kr-1", Labels: map[string]string{"rack": "r1"}, Address: "10.0.0.1", LastSeen: now},
		},
		fleet: &fakeFleet{
			vms: map[string][]console.MicroVM{
				"host-a": {{Name: "web-1", State: "running", Group: "web", Image: "reg/web@sha256:aa", Network: "lab"}},
				"host-b": {{Name: "db-0", State: "stopped", Group: "db", Image: "reg/db@sha256:bb"}},
			},
			groups: []console.Group{{Name: "web", Count: 3, Ready: 2}, {Name: "db", Count: 1, Ready: 1, Stateful: true}},
		},
		events: []orchapi.Event{
			{Time: now.Add(-time.Minute), Type: "host.enrolled", Subject: "host-a"},
			{Time: now, Type: "host.admitted", Subject: "host-b", Detail: map[string]string{"region": "kr-2"}},
		},
	}
}

func TestPages_RenderWhatOrchHolds(t *testing.T) {
	e := start(t, sampleOrch(), nil)
	tests := []struct {
		path string
		want []string
	}{
		{"/hosts", []string{"host-a", "kr-1", "rack=r1", "10.0.0.1", "host-b", "kr-2", ">draining<", ">active<"}},
		{"/microvms", []string{"host-a", "web-1", "reg/web@sha256:aa", ">lab<", "host-b", "db-0", "stopped"}},
		{"/groups", []string{"web", "db", ">2<", ">3<", "yes"}},
		{"/events", []string{"host.enrolled", "host.admitted", "region=kr-2"}},
		{"/pending", []string{"No pending hosts."}},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			status, body := e.get(t, tc.path)
			require.Equal(t, http.StatusOK, status)
			for _, w := range tc.want {
				assert.Contains(t, body, w)
			}
		})
	}
	t.Run("hosts are listed in name order", func(t *testing.T) {
		_, body := e.get(t, "/hosts")
		assert.Less(t, strings.Index(body, "host-a"), strings.Index(body, "host-b"))
	})
}

func TestMicroVMs_PerHost(t *testing.T) {
	orch := sampleOrch()
	orch.fleet.vmErr = map[string]error{"host-b": errors.New("host unreachable")}
	e := start(t, orch, nil)

	_, all := e.get(t, "/microvms")
	assert.Contains(t, all, "web-1")
	assert.Contains(t, all, "host unreachable", "a host that fails shows its error beside the hosts that answered")

	_, one := e.get(t, "/microvms?host=host-a")
	assert.Contains(t, one, "web-1")
	assert.NotContains(t, one, "host-b")
}

func TestPages_OrchDown(t *testing.T) {
	orch := &failingOrch{}
	ca := newAuthority(t)
	person := ca.person(t, "operator")
	s, err := console.New(console.Config{
		Orch: orch, Fleet: orch, AdmissionTTL: time.Hour,
		Sign: func(a identity.Admission) ([]byte, error) { return identity.SignAdmission(a, person.Key, person.Cert) },
	})
	require.NoError(t, err)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	for _, p := range []string{"/hosts", "/microvms", "/groups", "/events", "/pending"} {
		resp, err := http.Get(srv.URL + p)
		require.NoError(t, err)
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusBadGateway, resp.StatusCode, p)
		assert.Contains(t, string(b), "orch is down", p)
	}
}

type failingOrch struct {
	fakeOrch
	fakeFleet
}

var errDown = errors.New("orch is down")

func (*failingOrch) Hosts(context.Context) ([]orchapi.Host, error)      { return nil, errDown }
func (*failingOrch) Events(context.Context) ([]orchapi.Event, error)    { return nil, errDown }
func (*failingOrch) Pending(context.Context) ([]orchapi.Pending, error) { return nil, errDown }
func (f *failingOrch) Groups(context.Context) ([]console.Group, error)  { return nil, errDown }

func pendingOrch(t *testing.T) (*fakeOrch, []byte) {
	t.Helper()
	der := hostKeyDER(t)
	o := sampleOrch()
	o.pending = []orchapi.Pending{{HostID: "host-new", PublicKey: der, FirstSeen: now}}
	return o, der
}

func TestPending_ShowsCodeOfTheKey(t *testing.T) {
	o, der := pendingOrch(t)
	e := start(t, o, nil)
	_, body := e.get(t, "/pending")
	assert.Contains(t, body, "host-new")
	assert.Contains(t, body, identity.PairingCode(der))
}

func TestAdmit_SignsLocallyAndPostsAStatementThatVerifies(t *testing.T) {
	o, der := pendingOrch(t)
	e := start(t, o, nil)
	status, loc := e.post(t, "/pending/admit", url.Values{
		"csrf": {e.csrf(t)}, "host": {"host-new"},
		"code":   {strings.ToLower(strings.ReplaceAll(identity.PairingCode(der), "-", " "))},
		"region": {"kr-1"}, "labels": {"rack=r1\nzone = a, tier=gold"},
	})
	require.Equal(t, http.StatusSeeOther, status)
	assert.Equal(t, "/pending?admitted=host-new", loc)

	require.Len(t, o.admitted, 1)
	// The statement carries a certificate the test authority issued, so a verifier that trusts that
	// authority accepts it: the signature is the operator's.
	id, adm, err := identity.VerifyAdmission(o.admitted[0].Statement, e.ca.pool, nil, now)
	require.NoError(t, err)
	assert.Equal(t, identity.RoleOperator, id.Role)
	assert.Equal(t, "alice", id.Subject)
	sum := sha256.Sum256(der)
	assert.Equal(t, "host-new", adm.HostID)
	assert.Equal(t, hex.EncodeToString(sum[:]), adm.HostKeySHA256, "the key is the one orch listed")
	assert.Equal(t, "kr-1", adm.Region)
	assert.Equal(t, map[string]string{"rack": "r1", "zone": "a", "tier": "gold"}, adm.Labels)
	assert.Equal(t, now.Add(time.Hour), adm.ExpiresAt)
}

func TestAdmit_Refusals(t *testing.T) {
	o, der := pendingOrch(t)
	e := start(t, o, nil)
	good := func() url.Values {
		return url.Values{"csrf": {e.csrf(t)}, "host": {"host-new"}, "code": {identity.PairingCode(der)}, "region": {"kr-1"}}
	}

	// The control: the same form, unchanged, is accepted, so each refusal below is its one change.
	status, _ := e.post(t, "/pending/admit", good())
	require.Equal(t, http.StatusSeeOther, status)
	require.Len(t, o.admitted, 1)
	o.admitted = nil

	tests := []struct {
		name   string
		change func(url.Values)
		status int
		body   string
	}{
		{"no CSRF token", func(v url.Values) { v.Del("csrf") }, http.StatusForbidden, "CSRF"},
		{"another CSRF token", func(v url.Values) { v.Set("csrf", strings.Repeat("0", 64)) }, http.StatusForbidden, "CSRF"},
		{"another host's code", func(v url.Values) { v.Set("code", identity.PairingCode(hostKeyDER(t))) }, http.StatusUnprocessableEntity, "does not match"},
		{"no code", func(v url.Values) { v.Del("code") }, http.StatusUnprocessableEntity, "does not match"},
		{"a host that is not pending", func(v url.Values) { v.Set("host", "host-a") }, http.StatusNotFound, "no such pending host"},
		{"no region", func(v url.Values) { v.Set("region", " ") }, http.StatusUnprocessableEntity, "region"},
		{"a label without a value", func(v url.Values) { v.Set("labels", "rack") }, http.StatusUnprocessableEntity, "key=value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := good()
			tc.change(v)
			status, body := e.post(t, "/pending/admit", v)
			assert.Equal(t, tc.status, status)
			assert.Contains(t, body, tc.body)
			assert.Empty(t, o.admitted, "nothing is signed and posted")
		})
	}
}

func TestAdmit_OrchRefusalIsShown(t *testing.T) {
	o, der := pendingOrch(t)
	o.admitErr = &orchapi.Error{Status: http.StatusConflict, Message: "host already admitted"}
	e := start(t, o, nil)
	status, body := e.post(t, "/pending/admit", url.Values{
		"csrf": {e.csrf(t)}, "host": {"host-new"}, "code": {identity.PairingCode(der)}, "region": {"kr-1"},
	})
	assert.Equal(t, http.StatusConflict, status)
	assert.Contains(t, body, "host already admitted")
}

func TestAdmit_SigningFailureIsRefused(t *testing.T) {
	o, der := pendingOrch(t)
	s, err := console.New(console.Config{
		Orch: o, AdmissionTTL: time.Hour,
		Sign: func(identity.Admission) ([]byte, error) { return nil, errors.New("key locked") },
	})
	require.NoError(t, err)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := env{orch: o, srv: srv}
	status, body := e.post(t, "/pending/admit", url.Values{
		"csrf": {e.csrf(t)}, "host": {"host-new"}, "code": {identity.PairingCode(der)}, "region": {"kr-1"},
	})
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.NotContains(t, body, "key locked", "the page does not repeat what the signer said")
	assert.Empty(t, o.admitted)
}

func TestCSRFToken_IsPerProcess(t *testing.T) {
	oa, _ := pendingOrch(t)
	ob, _ := pendingOrch(t)
	a := start(t, oa, nil)
	b := start(t, ob, nil)
	assert.NotEqual(t, a.csrf(t), b.csrf(t))
	assert.Equal(t, a.csrf(t), a.csrf(t), "one token for the life of the server")
}

func TestHostHeader_OnlyTheServersOwnIsServed(t *testing.T) {
	e := start(t, sampleOrch(), func(srvHost string) []string { return []string{srvHost} })

	status, _ := e.get(t, "/hosts")
	require.Equal(t, http.StatusOK, status, "the control: the server's own address is served")

	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/hosts", nil)
	require.NoError(t, err)
	req.Host = "attacker.example:" + strings.Split(e.srv.URL, ":")[2]
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestStatic_ServesTheStylesheetAndScriptOnly(t *testing.T) {
	e := start(t, sampleOrch(), nil)
	for _, f := range []string{"style.css", "app.js"} {
		status, body := e.get(t, "/static/"+f)
		assert.Equal(t, http.StatusOK, status, f)
		assert.NotEmpty(t, body, f)
	}
	for _, f := range []string{"pending.html", "base.html", "nothing"} {
		status, _ := e.get(t, "/static/"+f)
		assert.Equal(t, http.StatusNotFound, status, f)
	}
}

func TestPages_NeverCarryTheKey(t *testing.T) {
	o, der := pendingOrch(t)
	ca := newAuthority(t)
	person := ca.person(t, "operator")
	keyDER, err := x509.MarshalPKCS8PrivateKey(person.Key)
	require.NoError(t, err)
	s, err := console.New(console.Config{
		Orch: o, AdmissionTTL: time.Hour, Now: func() time.Time { return now },
		Sign: func(a identity.Admission) ([]byte, error) { return identity.SignAdmission(a, person.Key, person.Cert) },
	})
	require.NoError(t, err)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := env{orch: o, srv: srv}

	d := person.Key.(*ecdsa.PrivateKey).D.Bytes()
	for _, p := range []string{"/hosts", "/microvms", "/groups", "/events", "/pending", "/static/app.js", "/static/style.css"} {
		_, body := e.get(t, p)
		assert.NotContains(t, body, "PRIVATE KEY", p)
		assert.NotContains(t, body, hex.EncodeToString(d), p)
		assert.NotContains(t, body, hex.EncodeToString(keyDER), p)
	}
	_, body := e.post(t, "/pending/admit", url.Values{
		"csrf": {e.csrf(t)}, "host": {"host-new"}, "code": {identity.PairingCode(der)}, "region": {"kr-1"},
	})
	assert.NotContains(t, body, hex.EncodeToString(d))
	// What the operator's key produced is on its way to orch, and holds no key.
	require.Len(t, o.admitted, 1)
	assert.NotContains(t, string(o.admitted[0].Statement), "PRIVATE")
}

func TestListen_LoopbackOnly(t *testing.T) {
	tests := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:0", true},
		{"[::1]:0", true},
		{"0.0.0.0:0", false},
		{":0", false},
		{"localhost:0", false},
		{"not-an-address", false},
	}
	for _, tc := range tests {
		t.Run(tc.addr, func(t *testing.T) {
			ln, err := console.Listen(tc.addr)
			if ln != nil {
				_ = ln.Close()
			}
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestNew_RefusesAnIncompleteConfig(t *testing.T) {
	_, err := console.New(console.Config{AdmissionTTL: time.Hour})
	assert.Error(t, err)
	_, err = console.New(console.Config{Orch: &fakeOrch{}, Sign: func(identity.Admission) ([]byte, error) { return nil, nil }})
	assert.Error(t, err, "a TTL of zero would sign admissions that are already expired")
}

func TestFleetPages_SayWhenOrchDoesNotServeThem(t *testing.T) {
	o := sampleOrch()
	o.fleet = nil
	e := start(t, o, nil)
	for path, want := range map[string]string{"/microvms": "orch does not serve microVMs yet", "/groups": "orch does not serve groups yet"} {
		status, body := e.get(t, path)
		require.Equal(t, http.StatusOK, status, path)
		assert.Contains(t, body, want, path)
		assert.NotContains(t, body, "No such host", path)
	}
}

func TestEvents_NewestFirst(t *testing.T) {
	e := start(t, sampleOrch(), nil)
	_, body := e.get(t, "/events")
	assert.Less(t, strings.Index(body, "host.admitted"), strings.Index(body, "host.enrolled"))
}

func TestAdmit_PostsWhatOrchNeedsToCheck(t *testing.T) {
	o, der := pendingOrch(t)
	e := start(t, o, nil)
	status, _ := e.post(t, "/pending/admit", url.Values{
		"csrf": {e.csrf(t)}, "host": {"host-new"}, "code": {strings.ToLower(identity.PairingCode(der))},
		"region": {"kr-1"}, "labels": {"rack=r1"},
	})
	require.Equal(t, http.StatusSeeOther, status)
	require.Len(t, o.admitted, 1)
	assert.Equal(t, "host-new", o.admitted[0].HostID)
	assert.Equal(t, identity.PairingCode(der), o.admitted[0].Code, "orch compares the code against its own key")
	assert.Equal(t, "kr-1", o.admitted[0].Region)
	assert.Equal(t, map[string]string{"rack": "r1"}, o.admitted[0].Labels)
}
