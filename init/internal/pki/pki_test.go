package pki_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/pki"
	"github.com/garamsh/naru/init/internal/pkitest"
)

var epoch = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeClock hands every After to the test, which decides when it fires.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan *timer
}

type timer struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: epoch, timers: make(chan *timer, 16)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	tm := &timer{d: d, ch: make(chan time.Time, 1)}
	c.timers <- tm
	return tm.ch
}

// nextTimer returns the timer the issuer is waiting on.
func (c *fakeClock) nextTimer(t *testing.T) *timer {
	t.Helper()
	select {
	case tm := <-c.timers:
		return tm
	case <-time.After(5 * time.Second):
		t.Fatal("the issuer set no timer")
		panic("unreachable")
	}
}

// restarts records the services restarted, in order.
type restarts struct{ ch chan string }

func (r restarts) Restart(name string) { r.ch <- name }

func (r restarts) next(t *testing.T) string {
	t.Helper()
	select {
	case name := <-r.ch:
		return name
	case <-time.After(5 * time.Second):
		t.Fatal("no service was restarted")
		panic("unreachable")
	}
}

func (r restarts) none(t *testing.T) {
	t.Helper()
	select {
	case name := <-r.ch:
		t.Fatalf("service %s was restarted", name)
	default:
	}
}

type rig struct {
	root     string
	clock    *fakeClock
	restarts restarts
	log      *bytes.Buffer
	issuer   *pki.Issuer
	ca       pkitest.CA
	spec     config.PKI
}

func newRig(t *testing.T, certs ...config.Certificate) *rig {
	t.Helper()
	return newRigExpiring(t, epoch.Add(24*365*time.Hour), certs...)
}

// newRigExpiring is newRig with an authority whose certificate expires at notAfter.
func newRigExpiring(t *testing.T, notAfter time.Time, certs ...config.Certificate) *rig {
	t.Helper()
	r := &rig{root: t.TempDir(), clock: newFakeClock(), restarts: restarts{make(chan string, 16)}, log: &bytes.Buffer{}}
	r.ca = pkitest.NewCA(t, epoch.Add(-24*time.Hour), notAfter)
	r.spec = config.PKI{
		Authorities:  []config.Authority{{Name: "ca", Certificate: r.ca.Cert, Key: r.ca.Key, CertificatePath: "/run/pki/ca.pem"}},
		Certificates: certs,
	}
	r.issuer = pki.New(r.root, r.clock, r.restarts, slog.New(slog.NewTextHandler(r.log, nil)))
	return r
}

func (r *rig) path(p string) string { return filepath.Join(r.root, p) }

func (r *rig) readCert(t *testing.T, p string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(r.path(p))
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func (r *rig) readKey(t *testing.T, p string) *ecdsa.PrivateKey {
	t.Helper()
	data, err := os.ReadFile(r.path(p))
	require.NoError(t, err)
	block, _ := pem.Decode(data)
	require.NotNil(t, block)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	require.NoError(t, err)
	return key
}

func serverCert() config.Certificate {
	return config.Certificate{
		Name: "apiserver", Authority: "ca", CertificatePath: "/run/pki/apiserver.pem", KeyPath: "/run/pki/apiserver.key",
		CommonName: "apiserver", Usage: config.Server, NodeAddresses: true, DNSNames: []string{"api.example"},
		Validity: "24h", Restart: []string{"apiserver"},
	}
}

func clientCert() config.Certificate {
	return config.Certificate{
		Name: "agent", Authority: "ca", CertificatePath: "/run/pki/agent.pem", KeyPath: "/run/pki/agent.key",
		CommonName: "node-1", Organization: "nodes", Usage: config.Client, Validity: "24h", Restart: []string{"agent"},
	}
}

var nodeAddrs = []netip.Addr{netip.MustParseAddr("10.78.0.1"), netip.MustParseAddr("fd00::1")}

func TestIssue_WritesCertificatesTheAuthorityVerifies(t *testing.T) {
	r := newRig(t, serverCert(), clientCert())

	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(r.ca.Cert)))
	server := r.readCert(t, "/run/pki/apiserver.pem")
	_, err := server.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: epoch, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSName: "api.example"})
	assert.NoError(t, err)
	client := r.readCert(t, "/run/pki/agent.pem")
	_, err = client.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: epoch, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err)
	assert.Equal(t, "node-1", client.Subject.CommonName)
	assert.Equal(t, []string{"nodes"}, client.Subject.Organization)
	assert.True(t, server.NotAfter.Equal(epoch.Add(24*time.Hour)), "valid for the validity from now")
	assert.True(t, server.NotBefore.Before(epoch), "valid from before now")
	assert.True(t, r.readKey(t, "/run/pki/apiserver.key").PublicKey.Equal(server.PublicKey), "the key is the certificate's")
	assert.Equal(t, r.ca.Cert, string(mustRead(t, r.path("/run/pki/ca.pem"))), "the authority's certificate is written as given")
}

func TestIssue_WritesTheKeyPrivateAndTheCertificatesReadable(t *testing.T) {
	r := newRig(t, serverCert())

	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))

	for p, want := range map[string]os.FileMode{"/run/pki/apiserver.key": 0o600, "/run/pki/apiserver.pem": 0o644, "/run/pki/ca.pem": 0o644} {
		info, err := os.Stat(r.path(p))
		require.NoError(t, err)
		assert.Equal(t, want, info.Mode().Perm(), p)
	}
}

func TestIssue_WritesTheAuthorityKeyOnlyWhereTheAuthorityNamesAPath(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	assert.NoFileExists(t, r.path("/run/pki/ca.key"), "the control: without keyPath the key stays in memory")

	r = newRig(t, serverCert())
	r.spec.Authorities[0].KeyPath = "/run/pki/ca.key"
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))

	assert.Equal(t, r.ca.Key, string(mustRead(t, r.path("/run/pki/ca.key"))))
	info, err := os.Stat(r.path("/run/pki/ca.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestIssue_NamesTheNodeAddressesInTheCertificate(t *testing.T) {
	tests := map[string]struct {
		cert config.Certificate
		want []string
	}{
		"a server certificate gets the Node's addresses and 127.0.0.1": {serverCert(), []string{"10.78.0.1", "127.0.0.1", "fd00::1"}},
		"a client certificate gets the Node's addresses only": {
			func() config.Certificate { c := clientCert(); c.NodeAddresses = true; return c }(),
			[]string{"10.78.0.1", "fd00::1"},
		},
		"addresses named twice are listed once": {
			func() config.Certificate {
				c := serverCert()
				c.IPAddresses = []string{"10.78.0.1", "192.0.2.7"}
				return c
			}(),
			[]string{"10.78.0.1", "127.0.0.1", "192.0.2.7", "fd00::1"},
		},
		"without nodeAddresses the Node's addresses are not named": {
			func() config.Certificate { c := serverCert(); c.NodeAddresses = false; return c }(),
			nil,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, tt.cert)

			require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))

			var got []string
			for _, ip := range r.readCert(t, tt.cert.CertificatePath).IPAddresses {
				got = append(got, ip.String())
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIssue_LogsEachAuthorityAndCertificate(t *testing.T) {
	r := newRig(t, serverCert())

	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))

	assert.Contains(t, r.log.String(), "msg=\"authority written\" name=ca path=/run/pki/ca.pem")
	assert.Contains(t, r.log.String(), "msg=\"certificate issued\" name=apiserver authority=ca usage=server path=/run/pki/apiserver.pem expires=2030-01-02T00:00:00Z")
	assert.NotContains(t, r.log.String(), "PRIVATE", "a key is never logged")
}

func TestIssue_RefusesAnExpiredAuthorityAndWritesNoCertificate(t *testing.T) {
	// The control is the first test: the same specification issues while the authority is valid.
	r := newRig(t, serverCert())
	r.clock.advance(24*365*time.Hour + time.Hour)

	err := r.issuer.Issue(r.spec, nodeAddrs)

	require.Error(t, err)
	assert.ErrorContains(t, err, "not valid")
	assert.NoFileExists(t, r.path("/run/pki/apiserver.pem"))
	assert.NoFileExists(t, r.path("/run/pki/apiserver.key"))
}

func TestIssue_RefusesAPathThatExists(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, os.MkdirAll(r.path("/run/pki"), 0o755))
	require.NoError(t, os.WriteFile(r.path("/run/pki/apiserver.key"), []byte("operator's"), 0o600))

	err := r.issuer.Issue(r.spec, nodeAddrs)

	require.Error(t, err)
	assert.Equal(t, "operator's", string(mustRead(t, r.path("/run/pki/apiserver.key"))), "the file is not overwritten")
}

func TestRun_RenewsACertificateAtTwoThirdsOfItsValidity(t *testing.T) {
	r := newRig(t, serverCert(), clientCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	oldCert := r.readCert(t, "/run/pki/apiserver.pem")
	oldKey := r.readKey(t, "/run/pki/apiserver.key")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.issuer.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	first := r.clock.nextTimer(t)
	assert.Equal(t, 16*time.Hour, first.d, "two thirds of 24h")
	r.clock.advance(16 * time.Hour)
	first.ch <- time.Time{}

	assert.Equal(t, "apiserver", r.restarts.next(t))
	assert.Equal(t, "agent", r.restarts.next(t))
	newCert := r.readCert(t, "/run/pki/apiserver.pem")
	assert.NotEqual(t, oldCert.SerialNumber, newCert.SerialNumber, "a new certificate")
	assert.False(t, oldKey.PublicKey.Equal(newCert.PublicKey), "under a new key")
	assert.True(t, r.readKey(t, "/run/pki/apiserver.key").PublicKey.Equal(newCert.PublicKey))
	assert.True(t, newCert.NotAfter.Equal(epoch.Add(16*time.Hour+24*time.Hour)), "valid for the validity from the renewal")
	assert.Contains(t, r.log.String(), "msg=\"certificate renewed\" name=apiserver")
	assert.Equal(t, 16*time.Hour, r.clock.nextTimer(t).d, "the next renewal is two thirds of the new validity away")
}

func TestRun_RestartsOnlyTheServicesOfTheCertificateRenewed(t *testing.T) {
	short := clientCert()
	short.Validity = "6h"
	r := newRig(t, serverCert(), short)
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.issuer.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	first := r.clock.nextTimer(t)
	assert.Equal(t, 4*time.Hour, first.d, "the earliest renewal is the short certificate's")
	r.clock.advance(4 * time.Hour)
	first.ch <- time.Time{}

	assert.Equal(t, "agent", r.restarts.next(t))
	next := r.clock.nextTimer(t)
	assert.Equal(t, 4*time.Hour, next.d, "the short certificate is due again before the long one, which has 12h left")
	r.restarts.none(t)
}

func TestRun_DoesNotRenewBeforeTwoThirdsHavePassed(t *testing.T) {
	// The control is the first renewal test: the same timer firing at two thirds renews.
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.issuer.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	first := r.clock.nextTimer(t)
	r.clock.advance(time.Hour)
	first.ch <- time.Time{}

	assert.Equal(t, 15*time.Hour, r.clock.nextTimer(t).d, "it waits for what is left")
	r.restarts.none(t)
}

func TestRun_RetriesARenewalThatFailsAndRestartsNothingUntilItSucceeds(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	oldCert := r.readCert(t, "/run/pki/apiserver.pem")
	require.NoError(t, os.Rename(r.path("/run/pki"), r.path("/run/pki-away")))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.issuer.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	first := r.clock.nextTimer(t)
	r.clock.advance(16 * time.Hour)
	first.ch <- time.Time{}
	retry := r.clock.nextTimer(t)

	assert.Equal(t, time.Minute, retry.d)
	assert.Contains(t, r.log.String(), "msg=\"certificate renewal failed\" name=apiserver")
	r.restarts.none(t)

	require.NoError(t, os.Rename(r.path("/run/pki-away"), r.path("/run/pki")))
	r.clock.advance(time.Minute)
	retry.ch <- time.Time{}

	assert.Equal(t, "apiserver", r.restarts.next(t))
	assert.NotEqual(t, oldCert.SerialNumber, r.readCert(t, "/run/pki/apiserver.pem").SerialNumber)
}

// runRenewal starts the issuer's renewal and stops it when the test ends.
func (r *rig) runRenewal(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.issuer.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestRun_RenewsAtTwoThirdsOfAValidityThatTwoTimesWouldOverflow(t *testing.T) {
	// The control is the first renewal test: the same arithmetic, on a validity it does not overflow on.
	long := serverCert()
	long.Validity = "1500000h" // 171 years: twice that is more than an int64 holds in nanoseconds
	r := newRigExpiring(t, epoch.Add(200*365*24*time.Hour), long)
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	r.runRenewal(t)

	assert.Equal(t, 1000000*time.Hour, r.clock.nextTimer(t).d, "two thirds of 1500000h")
}

func TestIssue_EndsALeafAtTheAuthoritiesExpiry(t *testing.T) {
	long := serverCert()
	long.Validity = "720h"
	r := newRigExpiring(t, epoch.Add(48*time.Hour), long)
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	r.runRenewal(t)

	assert.True(t, r.readCert(t, "/run/pki/apiserver.pem").NotAfter.Equal(epoch.Add(48*time.Hour)), "not past the authority, 720h being the validity")
	assert.Contains(t, r.log.String(), "msg=\"authority expires soon\" name=ca")
	first := r.clock.nextTimer(t)
	assert.Equal(t, 32*time.Hour, first.d, "two thirds of the 48h it lives, not of 720h")
	r.clock.advance(32 * time.Hour)
	first.ch <- time.Time{}

	assert.Equal(t, "apiserver", r.restarts.next(t))
	assert.True(t, r.readCert(t, "/run/pki/apiserver.pem").NotAfter.Equal(epoch.Add(48*time.Hour)), "the renewal is capped too")
	assert.Equal(t, 10*time.Hour+40*time.Minute, r.clock.nextTimer(t).d, "two thirds of the 16h left")
}

func TestIssue_RefusesALeafThatWouldLiveLessThanAnHour(t *testing.T) {
	// The control is the renewal test below: the same specification issues while the authority leaves two hours.
	r := newRigExpiring(t, epoch.Add(30*time.Minute), serverCert())

	err := r.issuer.Issue(r.spec, nodeAddrs)

	require.Error(t, err)
	assert.ErrorContains(t, err, "authority ca expires at 2030-01-01T00:30:00Z")
	assert.NoFileExists(t, r.path("/run/pki/apiserver.pem"))
	assert.Contains(t, r.log.String(), "msg=\"authority expires soon\"")
}

func TestRun_RefusesARenewalThatWouldLiveLessThanAnHourAndRetries(t *testing.T) {
	r := newRigExpiring(t, epoch.Add(2*time.Hour), serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	old := mustRead(t, r.path("/run/pki/apiserver.pem"))
	r.runRenewal(t)

	first := r.clock.nextTimer(t)
	assert.Equal(t, 80*time.Minute, first.d, "two thirds of the 2h it was issued for")
	r.clock.advance(80 * time.Minute)
	first.ch <- time.Time{}
	retry := r.clock.nextTimer(t)

	assert.Equal(t, time.Minute, retry.d)
	assert.Contains(t, r.log.String(), "msg=\"certificate renewal failed\" name=apiserver")
	assert.Contains(t, r.log.String(), "authority ca expires at 2030-01-01T02:00:00Z")
	assert.Equal(t, old, mustRead(t, r.path("/run/pki/apiserver.pem")), "the certificate stands")
	r.restarts.none(t)
}

func TestRun_LeavesTheOldPairWhenTheCertificateCannotBeReplaced(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	oldKey := mustRead(t, r.path("/run/pki/apiserver.key"))
	oldCert := mustRead(t, r.path("/run/pki/apiserver.pem"))
	// A directory where the certificate is: the new key is renamed in, and the new certificate cannot be.
	require.NoError(t, os.Remove(r.path("/run/pki/apiserver.pem")))
	require.NoError(t, os.Mkdir(r.path("/run/pki/apiserver.pem"), 0o755))
	r.runRenewal(t)

	first := r.clock.nextTimer(t)
	r.clock.advance(16 * time.Hour)
	first.ch <- time.Time{}
	retry := r.clock.nextTimer(t)

	assert.Contains(t, r.log.String(), "msg=\"certificate renewal failed\" name=apiserver")
	assert.Equal(t, oldKey, mustRead(t, r.path("/run/pki/apiserver.key")), "the old key is back beside the certificate")
	entries, err := os.ReadDir(r.path("/run/pki"))
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{"ca.pem", "apiserver.key", "apiserver.pem"}, names, "no staged file is left")
	r.restarts.none(t)

	// The control: the same renewal succeeds once the certificate's path is a file again.
	require.NoError(t, os.Remove(r.path("/run/pki/apiserver.pem")))
	require.NoError(t, os.WriteFile(r.path("/run/pki/apiserver.pem"), oldCert, 0o644))
	r.clock.advance(time.Minute)
	retry.ch <- time.Time{}

	assert.Equal(t, "apiserver", r.restarts.next(t))
	assert.NotEqual(t, oldKey, mustRead(t, r.path("/run/pki/apiserver.key")))
}

func TestRun_RestartsAServiceTwoCertificatesNameOnceAfterBothRenewals(t *testing.T) {
	first, second := serverCert(), clientCert()
	first.Restart = []string{"apiserver", "shared"}
	second.Restart = []string{"shared", "agent"}
	r := newRig(t, first, second)
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	r.runRenewal(t)

	timer := r.clock.nextTimer(t)
	r.clock.advance(16 * time.Hour)
	timer.ch <- time.Time{}

	assert.Equal(t, "apiserver", r.restarts.next(t))
	assert.Equal(t, "shared", r.restarts.next(t))
	assert.Equal(t, "agent", r.restarts.next(t), "in the order first named")
	r.clock.nextTimer(t)
	r.restarts.none(t)
}

func TestRun_ReturnsAtOnceWhenIssuingFailedAfterIssuingSome(t *testing.T) {
	// The control is the renewal tests: after a complete Issue the same Run waits for a renewal.
	missing := clientCert()
	missing.Authority = "nope"
	r := newRig(t, serverCert(), missing)
	require.Error(t, r.issuer.Issue(r.spec, nodeAddrs))
	require.FileExists(t, r.path("/run/pki/apiserver.pem"), "the first certificate was issued")
	done := make(chan struct{})

	go func() { defer close(done); r.issuer.Run(context.Background()) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run waits to renew a certificate of a refused boot")
	}
}

func TestRun_StopsWhenItsContextEnds(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.issuer.Run(ctx) }()
	r.clock.nextTimer(t)

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	r.restarts.none(t)
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	return data
}
