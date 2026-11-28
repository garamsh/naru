package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/api"
	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// ca is a throwaway authority: orch's host CA.
type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "host ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return ca{cert, key}
}

func (a ca) fingerprint() string {
	sum := sha256.Sum256(a.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// issue returns a certificate of the subject signed by a, as PEM files' contents and a tls pair.
func (a ca) issue(t *testing.T, subject pkix.Name, server bool) (certPEM, keyPEM []byte, pair tls.Certificate, leaf *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: new(big.Int).SetUint64(uint64(time.Now().UnixNano())), Subject: subject,
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
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
	pair, err = tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	leaf, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	return certPEM, keyPEM, pair, leaf
}

// fakeOrch serves what maintenance reads of orch: a TLS chain that holds the CA, and the signed list.
type fakeOrch struct {
	srv     *httptest.Server
	ca      ca
	key     ed25519.PrivateKey
	pub     []byte
	revoked atomic.Value // []string
	crlDown atomic.Bool
}

func newFakeOrch(t *testing.T) *fakeOrch {
	t.Helper()
	f := &fakeOrch{ca: newCA(t)}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	f.key = priv
	enc, err := identity.EncodeRevocationPublic(pub)
	require.NoError(t, err)
	f.pub = []byte(enc)
	f.revoked.Store([]string{})
	certPEM, keyPEM, _, _ := f.ca.issue(t, pkix.Name{CommonName: "orch"}, true)
	srvPair, err := tls.X509KeyPair(append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.ca.cert.Raw})...), keyPEM)
	require.NoError(t, err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/crl", func(w http.ResponseWriter, _ *http.Request) {
		if f.crlDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		signed, err := identity.SignCRL(f.key, identity.CRL{Seq: 1, IssuedAt: time.Now(), Serials: f.revoked.Load().([]string)})
		require.NoError(t, err)
		_, _ = w.Write(signed)
	})
	f.srv = httptest.NewUnstartedServer(mux)
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{srvPair}}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

// maintenance is a running hostd in maintenance mode.
type maintenance struct {
	addr     string
	orch     *fakeOrch
	marker   string
	rebooted chan struct{}
}

// startMaintenance runs runMaintenance over a host key dir that holds an enrolled pair named hostID.
func startMaintenance(t *testing.T, o *fakeOrch, hostID string) *maintenance {
	t.Helper()
	dir := t.TempDir()
	certPEM, keyPEM, _, _ := o.ca.issue(t, pkix.Name{CommonName: hostID}, true)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "host.crt"), certPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "host.key"), keyPEM, 0o600))
	pubFile := filepath.Join(dir, "rev.pub")
	require.NoError(t, os.WriteFile(pubFile, o.pub, 0o644))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	m := &maintenance{addr: addr, orch: o, marker: filepath.Join(dir, "reset.json"), rebooted: make(chan struct{}, 1)}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runMaintenance(ctx, maintenanceOptions{
			listen: addr, orchURL: o.srv.URL, orchCA: o.ca.fingerprint(), hostKey: filepath.Join(dir, "host.key"),
			reason: "no valid stored configuration", resetMarker: m.marker,
			reboot: func() error { m.rebooted <- struct{}{}; return nil },
			revocation: revocationConfig{
				keyFile: pubFile, maxAge: time.Hour, orchURL: o.srv.URL, orchCA: o.ca.fingerprint(), file: filepath.Join(dir, "crl.json"),
			},
		}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() { stop(); <-done })
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}, 5*time.Second, 20*time.Millisecond)
	return m
}

func (m *maintenance) client(pair tls.Certificate) *hostapi.Client {
	pool := x509.NewCertPool()
	pool.AddCert(m.orch.ca.cert)
	return hostapi.NewClient("https://"+m.addr, &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{pair},
	}}})
}

func operator(t *testing.T, o *fakeOrch) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	_, _, pair, leaf := o.ca.issue(t, pkix.Name{CommonName: "operator", Organization: []string{api.ProvisionerOrganization}}, false)
	return pair, leaf
}

func TestMaintenance_AnOperatorUnderOrchsHostCAReadsTheReportAndResets(t *testing.T) {
	o := newFakeOrch(t)
	m := startMaintenance(t, o, "host-7")
	pair, _ := operator(t, o)
	c := m.client(pair)

	got, err := c.Maintenance(t.Context())
	require.NoError(t, err)
	assert.Equal(t, hostapi.Maintenance{HostID: "host-7", Reason: "no valid stored configuration"}, got, "the ID is the enrolled certificate's common name")

	_, err = c.Reset(t.Context(), hostapi.ResetRequest{Scope: hostapi.ResetStore, Confirm: "host-7"})
	require.NoError(t, err)
	b, err := os.ReadFile(m.marker)
	require.NoError(t, err)
	assert.JSONEq(t, `{"scope":"store"}`, string(b))
	select {
	case <-m.rebooted:
	case <-time.After(5 * time.Second):
		t.Fatal("the host did not reboot")
	}
}

func TestMaintenance_ARevokedOperatorCertificateIsRefusedAndAnotherIsNot(t *testing.T) {
	o := newFakeOrch(t)
	pair, leaf := operator(t, o)
	other, _ := operator(t, o)
	o.revoked.Store([]string{identity.SerialOf(leaf)})
	m := startMaintenance(t, o, "host-7")

	_, err := m.client(pair).Maintenance(t.Context())
	assert.Error(t, err, "the revoked certificate is refused")

	_, err = m.client(other).Maintenance(t.Context())
	assert.NoError(t, err, "the control: a certificate of the same authority that is not on the list is accepted")
}

func TestMaintenance_ACertificateOfAnotherAuthorityIsRefused(t *testing.T) {
	o := newFakeOrch(t)
	m := startMaintenance(t, o, "host-7")
	foreign := newCA(t)
	_, _, pair, _ := foreign.issue(t, pkix.Name{CommonName: "operator", Organization: []string{api.ProvisionerOrganization}}, false)

	_, err := m.client(pair).Maintenance(t.Context())

	assert.Error(t, err)
}

func TestMaintenance_WithNoRevocationListEveryClientIsRefused(t *testing.T) {
	o := newFakeOrch(t)
	o.crlDown.Store(true)
	m := startMaintenance(t, o, "host-7")
	pair, _ := operator(t, o)

	_, err := m.client(pair).Maintenance(t.Context())
	require.Error(t, err, "the list cannot be had, so no client is trusted")
}

func TestMaintenance_NeedsTheAnchorOrchAndTheRevocationKey(t *testing.T) {
	err := runMaintenance(t.Context(), maintenanceOptions{orchURL: "https://orch:7500"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.ErrorContains(t, err, "maintenance needs")
}

func TestFetchAuthority_TakesTheCertificatesOfThePinnedAuthorityAndNoOther(t *testing.T) {
	o := newFakeOrch(t)
	want := x509.NewCertPool()
	want.AddCert(o.ca.cert)

	got, err := fetchAuthority(t.Context(), o.srv.URL, orchapi.KeyPin(o.ca.cert))
	require.NoError(t, err)
	assert.True(t, want.Equal(got), "the authority the key pin names")
	got, err = fetchAuthority(t.Context(), o.srv.URL, o.ca.fingerprint())
	require.NoError(t, err)
	assert.True(t, want.Equal(got), "and, until v0.1.0-rc.1, the certificate's hash")

	_, err = fetchAuthority(t.Context(), o.srv.URL, newCA(t).fingerprint())
	assert.Error(t, err, "another authority's fingerprint")
}

func TestHostIdentity_FlagThenEnrolledCertificateThenHostname(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "host.key")
	host, err := os.Hostname()
	require.NoError(t, err)

	id, err := hostIdentity("", key)
	require.NoError(t, err)
	assert.Equal(t, host, id, "no certificate yet")

	certPEM, _, _, _ := newCA(t).issue(t, pkix.Name{CommonName: "host-9"}, true)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "host.crt"), certPEM, 0o644))
	id, _ = hostIdentity("", key)
	assert.Equal(t, "host-9", id)

	id, _ = hostIdentity("flag-id", key)
	assert.Equal(t, "flag-id", id)
}

func TestPairAfterReset_IsTheConfigurationsPairBecauseTheClearDeletesTheStoredOne(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfgCrt, cfgKey := pairFiles(t, dir, "config", 0x2a)
	storedCrt, storedKey := pairFiles(t, dir, "stored", 0x2b)
	storedPath := filepath.Join(dir, "serving.pem")
	require.NoError(t, os.WriteFile(storedPath, append(mustRead(t, storedCrt), mustRead(t, storedKey)...), 0o600))
	list := listRevoking(t, "2a") // the configuration's pair is revoked, the stored one is not

	s, err := chooseServing(t.Context(), list, log, cfgCrt, cfgKey, storedPath)
	require.NoError(t, err, "the control: before the clear the stored pair serves")
	require.Equal(t, storedPath, s.File())

	err = pairAfterReset(list, cfgCrt)(t.Context())
	assert.ErrorIs(t, err, identity.ErrRevoked, "a data scope is refused: after it the configuration's pair serves")
	require.NoError(t, os.Remove(storedPath)) // what the clear does
	_, err = chooseServing(t.Context(), list, log, cfgCrt, cfgKey, storedPath)
	assert.ErrorIs(t, err, identity.ErrRevoked, "and the host start would refuse it, as the check said")

	assert.NoError(t, pairAfterReset(listRevoking(t), cfgCrt)(t.Context()), "an unrevoked pair is no refusal")
}

func TestServingPair_UsesTheRuleOfTheHostAPI(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hostKey := filepath.Join(dir, "host.key")
	crt, key := pairFiles(t, dir, "host", 0x2a)
	require.NoError(t, os.Rename(key, hostKey))
	require.NoError(t, os.Rename(crt, enroll.CertPathFor(hostKey)))

	_, err := servingPair(t.Context(), listRevoking(t), hostKey, log)
	require.NoError(t, err, "the control: the enrolled pair serves")
	_, err = servingPair(t.Context(), listRevoking(t, "2a"), hostKey, log)
	assert.ErrorIs(t, err, identity.ErrRevoked, "a revoked enrolled pair is not served, whatever a self-signed one would allow")
}
