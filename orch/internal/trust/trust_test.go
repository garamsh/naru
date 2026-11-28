package trust_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/trust"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

type events struct {
	mu   sync.Mutex
	got  []orchapi.Event
	fail bool
}

func (e *events) AppendEvent(_ context.Context, ev orchapi.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail {
		return errors.New("etcd is away")
	}
	e.got = append(e.got, ev)
	return nil
}

type hosts []trust.Host

func (h hosts) Trust(context.Context) ([]trust.Host, error) { return h, nil }

func authorityPEM(t *testing.T, key *ecdsa.PrivateKey, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(notAfter.Unix()), Subject: pkix.Name{CommonName: "naru-orch-ca"},
		NotBefore: notAfter.AddDate(-10, 0, 0), NotAfter: notAfter, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestAMembersPoolIsWarnedOfOnceAYearBeforeItEndsAndNotOnceItTookTheRenewedCertificate(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	end := time.Date(2036, 11, 28, 0, 0, 0, 0, time.UTC)
	file := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(file, authorityPEM(t, key, end), 0o600))

	var logged bytes.Buffer
	c := &clock{now: end.AddDate(-1, 0, 0).Add(-time.Hour)}
	ev := &events{}
	w := trust.New(trust.Config{Member: "orch-1", Pools: map[string]string{"ca": file}, Events: ev, Clock: c, Log: slog.New(slog.NewTextHandler(&logged, nil))})

	require.NoError(t, w.Check(context.Background()))
	assert.Empty(t, ev.got, "more than a year remains")
	assert.Empty(t, logged.String())

	c.now = end.AddDate(-1, 0, 0)
	require.NoError(t, w.Check(context.Background()))
	require.Len(t, ev.got, 1)
	assert.Equal(t, orchapi.Event{Time: c.now, Type: trust.EventExpiring, Subject: "orch/orch-1",
		Detail: map[string]string{"pool": "ca", "file": file, "authority": "naru-orch-ca", "expiresAt": "2036-11-28T00:00:00Z"}}, ev.got[0])
	assert.Contains(t, logged.String(), "level=WARN")
	assert.Contains(t, logged.String(), "expires_at=2036-11-28T00:00:00Z")
	assert.Contains(t, logged.String(), "authority=naru-orch-ca")

	require.NoError(t, w.Check(context.Background()))
	assert.Len(t, ev.got, 1, "a pool is warned of once")

	// The pool takes the renewed certificate: it no longer ends within a year.
	require.NoError(t, os.WriteFile(file, append(authorityPEM(t, key, end), authorityPEM(t, key, end.AddDate(8, 0, 0))...), 0o600))
	logged.Reset()
	require.NoError(t, w.Check(context.Background()))
	assert.Len(t, ev.got, 1)
	assert.Empty(t, logged.String())
}

func TestAHostsReportedPoolIsWarnedOfAndAFailedAppendIsTriedAgain(t *testing.T) {
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	c := &clock{now: now}
	ev := &events{fail: true}
	w := trust.New(trust.Config{
		Hosts:  hosts{{ID: "host-1", ExpiresAt: now.AddDate(0, 6, 0), Authority: "naru-host-ca"}, {ID: "host-2", ExpiresAt: now.AddDate(5, 0, 0)}},
		Events: ev, Clock: c, Log: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, w.Check(context.Background()))
	assert.Empty(t, ev.got)

	ev.fail = false
	require.NoError(t, w.Check(context.Background()))
	require.Len(t, ev.got, 1, "the append that failed is tried at the next check, and host-2 has years left")
	assert.Equal(t, "host-1", ev.got[0].Subject)
	assert.Equal(t, map[string]string{"pool": "client-ca", "authority": "naru-host-ca", "expiresAt": "2035-07-01T00:00:00Z"}, ev.got[0].Detail)
}

func TestAPoolThatCannotBeReadIsAnErrorAndTheOthersAreChecked(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	end := time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	good := filepath.Join(dir, "host-ca.crt")
	require.NoError(t, os.WriteFile(good, authorityPEM(t, key, end), 0o600))
	ev := &events{}
	w := trust.New(trust.Config{Member: "orch-2", Pools: map[string]string{"ca": filepath.Join(dir, "absent"), "host-ca": good},
		Events: ev, Clock: &clock{now: end}, Log: slog.New(slog.DiscardHandler)})
	require.Error(t, w.Check(context.Background()))
	require.Len(t, ev.got, 1)
	assert.Equal(t, "host-ca", ev.got[0].Detail["pool"], "a pool already past its end is warned of too")
}
