package orchd_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

type host struct {
	id  string
	key *ecdsa.PrivateKey
	pub []byte
	csr []byte
}

func newHost(t *testing.T, id string) host {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id}}, key)
	require.NoError(t, err)
	return host{id: id, key: key, pub: pub, csr: csr}
}

func (h host) request(secret string) orchapi.EnrollRequest {
	return orchapi.EnrollRequest{Key: secret, HostID: h.id, PublicKey: h.pub, CSR: h.csr, Address: "10.0.0.1:8443"}
}

// status is the HTTP status of an orch error, or 0 for success.
func status(err error) int {
	var e *orchapi.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// one is a single-member cluster with an operator, a reader and a root identity and an enrollment key.
type one struct {
	c        *orchtest.Cluster
	operator orchtest.Identity
	op       *orchapi.Client
	reader   *orchapi.Client
	anon     *orchapi.Client
	secret   string
}

func startOne(t *testing.T, opts orchtest.Options) *one {
	t.Helper()
	opts.Members = 1
	c := orchtest.Start(t, opts)
	o := &one{c: c, operator: c.CA.Issue(t, "alice", identity.RoleOperator)}
	reader := c.CA.Issue(t, "rita", identity.RoleReader)
	o.op = c.Client(t, 0, &o.operator)
	o.reader = c.Client(t, 0, &reader)
	o.anon = c.Client(t, 0, nil)
	k, err := o.op.CreateEnrollmentKey(t.Context(), c.Clock.Now().Add(time.Hour))
	require.NoError(t, err)
	o.secret = k.Secret
	return o
}

func TestEnrollThenAdmitIssuesTheHostItsCertificate(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "host-a")

	st, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)
	require.Equal(t, orchapi.StatePending, st.State)
	require.Equal(t, identity.PairingCode(h.pub), st.Code)

	// Until it is admitted the host polls and gets no certificate.
	st, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	require.Equal(t, orchapi.StatePending, st.State)
	require.Empty(t, st.Certificate)

	pending, err := o.op.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, identity.PairingCode(h.pub), pending[0].Code)

	got, err := o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: h.id, Code: pending[0].Code, Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul"})
	require.NoError(t, err)
	require.Equal(t, "kr-seoul", got.Region)

	st, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err)
	require.Equal(t, orchapi.StateAdmitted, st.State)
	block, _ := pem.Decode(st.Certificate)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.True(t, h.key.PublicKey.Equal(cert.PublicKey), "the certificate is for the host's own key")

	// The admitted host reports with its certificate.
	id := orchtest.Identity{TLS: tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: h.key, Leaf: cert}, Cert: cert}
	require.NoError(t, o.c.Client(t, 0, &id).Heartbeat(t.Context()))
	hosts, err := o.op.Hosts(t.Context())
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	require.False(t, hosts[0].LastSeen.IsZero())

	pending, err = o.op.Pending(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestPollWithoutProofOfTheKeyAnswersNothing(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "host-a")
	_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)

	_, err = o.anon.PollEnrollment(t.Context(), h.id, h.key, o.c.Clock.Now())
	require.NoError(t, err, "control: the key's holder polls")

	other := newHost(t, "host-a")
	_, err = o.anon.PollEnrollment(t.Context(), h.id, other.key, o.c.Clock.Now())
	require.Equal(t, http.StatusBadRequest, status(err), "a poll signed by another key")
}

func TestAMismatchedPairingCodeIsRefusedAndNothingIsSigned(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "host-a")
	_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)

	wrong := identity.PairingCode(newHost(t, "x").pub)
	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: h.id, Code: wrong, Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul"})
	require.Equal(t, http.StatusForbidden, status(err))
	require.Zero(t, o.c.Signer.Calls(), "orch refused before the signer was asked")
	pending, err := o.op.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1, "the host is still pending")

	// Control: the matching code, with everything else the same, admits.
	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul"})
	require.NoError(t, err)
	require.Equal(t, 1, o.c.Signer.Calls())
}

func TestTwoHostsFromOneKeyShowDifferentCodes(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	a, b := newHost(t, "host-a"), newHost(t, "host-b")
	for _, h := range []host{a, b} {
		_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
		require.NoError(t, err)
	}
	pending, err := o.op.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.NotEqual(t, pending[0].Code, pending[1].Code)
}

func TestEnrollmentIsRateLimited(t *testing.T) {
	t.Run("per source", func(t *testing.T) {
		o := startOne(t, orchtest.Options{Limits: inventory.Config{SourceRate: 0.001, SourceBurst: 2, KeyBurst: 1000, KeyRate: 1000}})
		for i := range 2 {
			_, err := o.anon.Enroll(t.Context(), newHost(t, hostName(i)).request(o.secret))
			require.NoError(t, err, "within the burst")
		}
		_, err := o.anon.Enroll(t.Context(), newHost(t, hostName(2)).request(o.secret))
		require.Equal(t, http.StatusTooManyRequests, status(err))

		// The bucket refills with time.
		o.c.Clock.Advance(20 * time.Minute)
		_, err = o.anon.Enroll(t.Context(), newHost(t, hostName(2)).request(o.secret))
		require.NoError(t, err)
	})
	t.Run("per key", func(t *testing.T) {
		o := startOne(t, orchtest.Options{Limits: inventory.Config{KeyRate: 0.001, KeyBurst: 2, SourceBurst: 1000, SourceRate: 1000}})
		for i := range 2 {
			_, err := o.anon.Enroll(t.Context(), newHost(t, hostName(i)).request(o.secret))
			require.NoError(t, err, "within the burst")
		}
		_, err := o.anon.Enroll(t.Context(), newHost(t, hostName(2)).request(o.secret))
		require.Equal(t, http.StatusTooManyRequests, status(err))

		// Control: the limit belongs to the key, not to the source, so another key still enrolls.
		k, err := o.op.CreateEnrollmentKey(t.Context(), o.c.Clock.Now().Add(time.Hour))
		require.NoError(t, err)
		_, err = o.anon.Enroll(t.Context(), newHost(t, hostName(3)).request(k.Secret))
		require.NoError(t, err)
	})
}

func hostName(i int) string { return "host-" + string(rune('a'+i)) }

func TestExpiredAndRevokedKeysAreRefused(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	_, err := o.anon.Enroll(t.Context(), newHost(t, "host-a").request(o.secret))
	require.NoError(t, err, "control: a live key enrolls")

	t.Run("expired", func(t *testing.T) {
		k, err := o.op.CreateEnrollmentKey(t.Context(), o.c.Clock.Now().Add(time.Minute))
		require.NoError(t, err)
		_, err = o.anon.Enroll(t.Context(), newHost(t, "host-b").request(k.Secret))
		require.NoError(t, err, "control: the key works before it expires")
		o.c.Clock.Advance(2 * time.Minute)
		_, err = o.anon.Enroll(t.Context(), newHost(t, "host-c").request(k.Secret))
		require.Equal(t, http.StatusForbidden, status(err))
	})
	t.Run("revoked", func(t *testing.T) {
		k, err := o.op.CreateEnrollmentKey(t.Context(), o.c.Clock.Now().Add(time.Hour))
		require.NoError(t, err)
		_, err = o.anon.Enroll(t.Context(), newHost(t, "host-d").request(k.Secret))
		require.NoError(t, err, "control: the key works before it is revoked")
		require.NoError(t, o.op.RevokeEnrollmentKey(t.Context(), k.ID))
		_, err = o.anon.Enroll(t.Context(), newHost(t, "host-e").request(k.Secret))
		require.Equal(t, http.StatusForbidden, status(err))
	})
	t.Run("unknown", func(t *testing.T) {
		_, err := o.anon.Enroll(t.Context(), newHost(t, "host-f").request("naru-ek-not-a-key"))
		require.Equal(t, http.StatusForbidden, status(err))
	})
}

func TestAdmissionNeedsAnOperatorWhoIsNotRevoked(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "host-a")
	_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)
	req := orchapi.AdmitRequest{HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul"}

	_, err = o.reader.Admit(t.Context(), req)
	require.Equal(t, http.StatusForbidden, status(err), "a reader")
	_, err = o.anon.Admit(t.Context(), req)
	require.Equal(t, http.StatusUnauthorized, status(err), "no certificate")

	o.c.CA.Revoke(o.operator)
	_, err = o.op.Admit(t.Context(), req)
	require.Equal(t, http.StatusForbidden, status(err), "a revoked operator")

	// Control: another operator admits the same host with the same request shape.
	bob := o.c.CA.Issue(t, "bob", identity.RoleOperator)
	req.Statement = orchtest.Statement(t, bob, h.id, h.pub)
	_, err = o.c.Client(t, 0, &bob).Admit(t.Context(), req)
	require.NoError(t, err)
}

func TestRegionAndLabelsAreSetAndListed(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	h := newHost(t, "host-a")
	_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)
	_, err = o.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: h.id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, o.operator, h.id, h.pub), Region: "kr-seoul"})
	require.NoError(t, err)

	_, err = o.reader.SetLabels(t.Context(), h.id, orchapi.SetLabelsRequest{Region: "kr-busan"})
	require.Equal(t, http.StatusForbidden, status(err), "a reader may not label")

	_, err = o.op.SetLabels(t.Context(), h.id, orchapi.SetLabelsRequest{Region: "kr-busan", Labels: map[string]string{"disk": "nvme", "tier": "edge"}})
	require.NoError(t, err)
	hosts, err := o.reader.Hosts(t.Context())
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	require.Equal(t, "kr-busan", hosts[0].Region)
	require.Equal(t, map[string]string{"disk": "nvme", "tier": "edge"}, hosts[0].Labels)

	_, err = o.op.SetLabels(t.Context(), h.id, orchapi.SetLabelsRequest{Region: "Not A Region"})
	require.Equal(t, http.StatusBadRequest, status(err))

	events, err := o.reader.Events(t.Context())
	require.NoError(t, err)
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	require.Contains(t, types, inventory.EventAdmitted)
	require.Contains(t, types, inventory.EventLabeled)
}

func TestSnapshotIsRootOnlyAndSealed(t *testing.T) {
	rec, err := identity.GenerateRecovery()
	require.NoError(t, err)
	o := startOne(t, orchtest.Options{Sealer: snapshot.SealTo(rec.X25519.PublicKey())})
	root := o.c.CA.Issue(t, "root", identity.RoleRoot)

	_, err = o.op.Snapshot(t.Context())
	require.Equal(t, http.StatusForbidden, status(err), "an operator")

	res, err := o.c.Client(t, 0, &root).Snapshot(t.Context())
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(o.c.SnapshotDir(0), res.File))
	require.NoError(t, err)
	plain, err := identity.Open(rec.X25519, b, []byte(snapshot.Info))
	require.NoError(t, err, "the recovery key opens the file")
	require.Contains(t, string(plain), "/naru/orch/", "control: the opened snapshot holds the inventory")
	require.NotContains(t, string(b), "/naru/orch/", "the file on disk holds none of it in the clear")
	require.Equal(t, int64(len(b)), res.Size)
}
