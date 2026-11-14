package command_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
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
	"github.com/garamsh/naru/lib/orchapi"
)

// fakeOrch records what naructl sends and answers the endpoints it calls.
type fakeOrch struct {
	srv     *httptest.Server
	mu      sync.Mutex
	pending []orchapi.Pending
	admits  []orchapi.AdmitRequest
	revoked []string
	groups  []orchapi.GroupSpec
	deleted []string
	// networks and deletedNetworks are what the network commands sent.
	networks        []sentNetwork
	deletedNetworks []string
}

// sentNetwork is a network as a PUT carried it.
type sentNetwork struct {
	name string
	spec orchapi.NetworkSpec
}

func (f *fakeOrch) lastAdmit(t *testing.T) orchapi.AdmitRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.admits, 1)
	return f.admits[0]
}

func newFakeOrch(t *testing.T, pending ...orchapi.Pending) *fakeOrch {
	t.Helper()
	f := &fakeOrch{pending: pending}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/pending", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(f.pending) })
	mux.HandleFunc("POST /v1/admit", func(w http.ResponseWriter, r *http.Request) {
		var req orchapi.AdmitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.admits = append(f.admits, req)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(orchapi.Host{ID: req.HostID, Region: req.Region, Labels: req.Labels})
	})
	mux.HandleFunc("POST /v1/enrollment-keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(orchapi.EnrollmentKey{ID: "0123456789abcdef", Secret: "naru-ek-shown-once", ExpiresAt: time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("DELETE /v1/enrollment-keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PathValue("id"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /v1/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		var spec orchapi.GroupSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.groups = append(f.groups, spec)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(orchapi.Group{Spec: spec, Hash: "h1", Observed: true, Members: []orchapi.GroupMember{}})
	})
	mux.HandleFunc("GET /v1/groups", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]orchapi.Group{{Spec: orchapi.GroupSpec{Name: "web", Region: "kr-seoul", Count: 2}, Members: []orchapi.GroupMember{{ID: "web-1", Phase: orchapi.PhaseRunning}, {ID: "web-2", Phase: orchapi.PhasePending}}}})
	})
	mux.HandleFunc("GET /v1/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(orchapi.Group{Spec: orchapi.GroupSpec{Name: r.PathValue("name"), Region: "kr-seoul", Count: 1}, Hash: "h1", Observed: true, Members: []orchapi.GroupMember{{ID: "web-1", Host: "host-a", TemplateHash: "h1", Phase: orchapi.PhaseRunning}}})
	})
	mux.HandleFunc("DELETE /v1/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("name"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /v1/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		var spec orchapi.NetworkSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.networks = append(f.networks, sentNetwork{r.PathValue("name"), spec})
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(orchapi.Network{Name: r.PathValue("name"), Prefix: spec.Prefix, APIEndpoint: spec.APIEndpoint})
	})
	mux.HandleFunc("GET /v1/networks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]orchapi.Network{{Name: "edge", Prefix: "10.8.0.0/24"}, {Name: "lab", Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2"}})
	})
	mux.HandleFunc("GET /v1/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(orchapi.Network{Name: r.PathValue("name"), Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2"})
	})
	mux.HandleFunc("DELETE /v1/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deletedNetworks = append(f.deletedNetworks, r.PathValue("name"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// operator is a throwaway authority and an operator it issued a certificate to, with the person
// file and passphrase file naructl reads.
type operator struct {
	pool                         *x509.CertPool
	person, passphrase, orchAuth string
}

func newOperator(t *testing.T, f *fakeOrch, role string) operator {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "alice", OrganizationalUnit: []string{role}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)

	dir := t.TempDir()
	pass := filepath.Join(dir, "pass")
	require.NoError(t, os.WriteFile(pass, []byte("correct horse\n"), 0o600))
	enc, err := identity.EncryptPerson("correct horse", identity.Person{Key: key, Cert: cert})
	require.NoError(t, err)
	person := filepath.Join(dir, "alice.person")
	require.NoError(t, os.WriteFile(person, enc, 0o600))
	orchCA := filepath.Join(dir, "orch-ca.pem")
	require.NoError(t, os.WriteFile(orchCA, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o644))
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return operator{pool: pool, person: person, passphrase: pass, orchAuth: orchCA}
}

func (o operator) args(f *fakeOrch, cmd ...string) []string {
	return append(cmd, "--orch-url", f.srv.URL, "--orch-ca", o.orchAuth, "--identity", o.person, "--passphrase-file", o.passphrase)
}

func hostKey(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	return der
}

func pendingHost(id string, pub []byte) orchapi.Pending {
	return orchapi.Pending{HostID: id, Code: identity.PairingCode(pub), PublicKey: pub, Address: "10.0.0.5:7443", FirstSeen: time.Now()}
}

func TestHostAdmit_SignsTheAdmissionOverTheKeyOrchHoldsAndPassesIt(t *testing.T) {
	pub := hostKey(t)
	f := newFakeOrch(t, pendingHost("host-a", pub))
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "host", "admit", "host-a", "--code", identity.PairingCode(pub), "--region", "kr-seoul", "--label", "disk=nvme")...)
	require.NoError(t, err, out)

	req := f.lastAdmit(t)
	assert.Equal(t, "host-a", req.HostID)
	assert.Equal(t, identity.PairingCode(pub), req.Code)
	by, adm, err := identity.VerifyAdmission(req.Statement, op.pool, nil, time.Now())
	require.NoError(t, err, "the statement verifies under lib/identity")
	sum := sha256.Sum256(pub)
	assert.Equal(t, identity.RoleOperator, by.Role)
	assert.Equal(t, hex.EncodeToString(sum[:]), adm.HostKeySHA256)
	assert.Equal(t, "kr-seoul", adm.Region)
	assert.Equal(t, map[string]string{"disk": "nvme"}, adm.Labels)
}

func TestHostAdmit_AWrongCodeSignsNothing(t *testing.T) {
	pub := hostKey(t)
	f := newFakeOrch(t, pendingHost("host-a", pub))
	op := newOperator(t, f, "operator")

	other := identity.PairingCode(hostKey(t))
	out, err := run(t, op.args(f, "host", "admit", "host-a", "--code", other, "--region", "kr-seoul")...)
	require.Error(t, err)
	assert.Contains(t, out, "does not belong")
	assert.Empty(t, f.admits, "orch was never asked")

	// Control: the same command with the right code admits, in any case and without the hyphen.
	right := identity.PairingCode(pub)
	loose := right[:4] + right[5:]
	out, err = run(t, op.args(f, "host", "admit", "host-a", "--code", loose, "--region", "kr-seoul")...)
	require.NoError(t, err, out)
	assert.Len(t, f.admits, 1)
}

func TestHostAdmit_AHostThatIsNotPendingIsRefused(t *testing.T) {
	f := newFakeOrch(t, pendingHost("host-a", hostKey(t)))
	op := newOperator(t, f, "operator")
	out, err := run(t, op.args(f, "host", "admit", "host-z", "--code", "AAAA-AAAA", "--region", "kr-seoul")...)
	require.Error(t, err)
	assert.Contains(t, out, "not pending")
	assert.Empty(t, f.admits)
}

func TestHostPending_ListsTheCodes(t *testing.T) {
	pub := hostKey(t)
	f := newFakeOrch(t, pendingHost("host-a", pub))
	op := newOperator(t, f, "operator")
	out, err := run(t, op.args(f, "host", "pending")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "host-a")
	assert.Contains(t, out, identity.PairingCode(pub))
}

func TestEnrollKey_CreatePrintsTheSecretAndRevokeSendsTheID(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")
	out, err := run(t, op.args(f, "enroll-key", "create", "--ttl", "1h")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "naru-ek-shown-once")

	out, err = run(t, op.args(f, "enroll-key", "revoke", "0123456789abcdef")...)
	require.NoError(t, err, out)
	assert.Equal(t, []string{"0123456789abcdef"}, f.revoked)
}

func TestTheOtherHostCommandIsStillTheHostAPIs(t *testing.T) {
	out, err := run(t, "host")
	require.Error(t, err)
	assert.Contains(t, out, "config", "host without a subcommand still wants the host API's client file")
}

func TestOrchInit_WritesTheRecoveryKeyEncryptedAndRefusesToReplaceIt(t *testing.T) {
	dir := t.TempDir()
	pass := filepath.Join(dir, "pass")
	require.NoError(t, os.WriteFile(pass, []byte("a long passphrase\n"), 0o600))
	out := filepath.Join(dir, "out")
	mustRun(t, "orch", "init", "--out-dir", out, "--passphrase-file", pass)

	st, err := os.Stat(filepath.Join(out, "recovery.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	enc, err := os.ReadFile(filepath.Join(out, "recovery.key"))
	require.NoError(t, err)
	_, err = identity.DecryptRecovery("a long passphrase", enc)
	require.NoError(t, err, "the passphrase opens it")
	_, err = identity.DecryptRecovery("not the passphrase", enc)
	require.Error(t, err)
	pubData, err := os.ReadFile(filepath.Join(out, "recovery.pub"))
	require.NoError(t, err)
	_, err = identity.ParseRecoveryPublic(pubData)
	require.NoError(t, err)

	text, err := run(t, "orch", "init", "--out-dir", out, "--passphrase-file", pass)
	require.Error(t, err, "a second key would orphan the backups")
	assert.Contains(t, text, "orphan")
}

func TestOrchRestore_OpensASealedSnapshotWithTheRecoveryKey(t *testing.T) {
	dir := t.TempDir()
	pass := filepath.Join(dir, "pass")
	require.NoError(t, os.WriteFile(pass, []byte("a long passphrase\n"), 0o600))
	mustRun(t, "orch", "init", "--out-dir", dir, "--passphrase-file", pass)
	pubData, err := os.ReadFile(filepath.Join(dir, "recovery.pub"))
	require.NoError(t, err)
	pub, err := identity.ParseRecoveryPublic(pubData)
	require.NoError(t, err)
	seal, err := pub.SealKey()
	require.NoError(t, err)
	sealed, err := identity.Seal(seal, []byte("the inventory"), []byte(orchapi.SnapshotInfo))
	require.NoError(t, err)
	snap := filepath.Join(dir, "etcd.snap.sealed")
	require.NoError(t, os.WriteFile(snap, sealed, 0o600))

	out := filepath.Join(dir, "etcd.db")
	mustRun(t, "orch", "restore", "--snapshot", snap, "--recovery-key", filepath.Join(dir, "recovery.key"), "--passphrase-file", pass, "--out", out)
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "the inventory", string(got))

	// A snapshot sealed under another purpose does not open, and nothing is written.
	other, err := identity.Seal(seal, []byte("x"), []byte("another purpose"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(snap, other, 0o600))
	out2 := filepath.Join(dir, "etcd2.db")
	_, err = run(t, "orch", "restore", "--snapshot", snap, "--recovery-key", filepath.Join(dir, "recovery.key"), "--passphrase-file", pass, "--out", out2)
	require.Error(t, err)
	_, err = os.Stat(out2)
	assert.True(t, os.IsNotExist(err))
}
