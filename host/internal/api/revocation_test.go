package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// signerStub serves the list a signer would, or fails while down is set.
type signerStub struct {
	mu      sync.Mutex
	key     ed25519.PrivateKey
	seq     uint64
	revoked []string
	down    bool
}

func (s *signerStub) revoke(serial string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, serial)
	s.seq++
}

func (s *signerStub) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

func (s *signerStub) CRL(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return nil, errors.New("the signer is down")
	}
	return identity.SignCRL(s.key, identity.CRL{Seq: s.seq, IssuedAt: time.Now(), Serials: append([]string{}, s.revoked...)})
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func revokingEnv(t *testing.T) (*env, *signerStub, *clock, *identity.RevocationList) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	sg := &signerStub{key: priv, seq: 1}
	clk := &clock{t: time.Now()}
	list := &identity.RevocationList{
		Source: sg, Key: pub, TTL: time.Minute, MaxAge: 10 * time.Minute, Now: clk.now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, list), sg, clk, list
}

func serialOf(t *testing.T, pair tls.Certificate) string {
	t.Helper()
	c, err := x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err)
	return identity.SerialOf(c)
}

// refused reports whether the client's request fails before any response, as a handshake refusal does.
func (e *env) refused(c *http.Client) bool {
	resp, err := c.Get("https://" + e.addr + "/v1/host")
	if err != nil {
		return true
	}
	_ = resp.Body.Close()
	return false
}

func TestRevocation_ARevokedClientCertificateIsRefused(t *testing.T) {
	e, sg, clk, _ := revokingEnv(t)
	revokedPair, _, _ := e.ca.issue(t, []string{ReaderOrganization}, false)
	revoked := e.client(revokedPair)
	other := e.as(t, ReaderOrganization)
	require.False(t, e.refused(revoked), "control: the certificate is accepted before it is revoked")

	sg.revoke(serialOf(t, revokedPair))
	clk.advance(2 * time.Minute) // past the list's TTL, so the next handshake fetches
	require.True(t, e.refused(revoked), "the revoked certificate is refused")
	require.False(t, e.refused(other), "a certificate that is not on the list is still accepted")
}

func TestRevocation_AListOlderThanTheMaxAgeRefusesEveryClient(t *testing.T) {
	e, sg, clk, list := revokingEnv(t)
	c := e.as(t, ReaderOrganization)
	require.False(t, e.refused(c), "control: accepted while the list is fresh")

	sg.setDown(true)
	clk.advance(9 * time.Minute)
	require.False(t, e.refused(c), "control: the last list is kept while it is within the max age")
	clk.advance(2 * time.Minute)
	require.True(t, e.refused(c), "refused once the list cannot be refreshed and is older than the max age")
	require.Contains(t, list.Status().LastError, "the signer is down")

	sg.setDown(false)
	clk.advance(identity.DefaultRetryInterval + time.Second) // the failed refresh holds the next off
	require.False(t, e.refused(c), "a refresh that succeeds ends the refusal")
}

func TestRevocation_NoListAtAllRefusesEveryClient(t *testing.T) {
	e, sg, _, _ := revokingEnv(t)
	sg.setDown(true)
	require.True(t, e.refused(e.as(t, ReaderOrganization)))
}

func TestRevocation_TheListsAgeAndLastErrorAreMetrics(t *testing.T) {
	e, sg, clk, _ := revokingEnv(t)
	c := e.as(t, ReaderOrganization)
	require.False(t, e.refused(c))
	sg.setDown(true)
	clk.advance(2 * time.Minute)
	require.False(t, e.refused(c))

	out, err := testutil.GatherAndCount(e.metrics, "naru_hostd_revocation_list_age_seconds", "naru_hostd_revocation_list_seq", "naru_hostd_revocation_list_fetch_error")
	require.NoError(t, err)
	require.Equal(t, 3, out)
	mfs, err := e.metrics.Gather()
	require.NoError(t, err)
	var seen bool
	for _, mf := range mfs {
		if mf.GetName() == "naru_hostd_revocation_list_age_seconds" {
			require.InDelta(t, 120, mf.GetMetric()[0].GetGauge().GetValue(), 1)
			seen = true
		}
		if mf.GetName() == "naru_hostd_revocation_list_fetch_error" {
			require.True(t, strings.Contains(mf.GetMetric()[0].GetLabel()[0].GetValue(), "the signer is down"))
		}
	}
	require.True(t, seen)
}
