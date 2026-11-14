package pki_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

func (r *rig) startRun(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.issuer.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func TestReissue_ReplacesTheNamedPairAndRestartsNothing(t *testing.T) {
	r := newRig(t, serverCert(), clientCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	oldServer := r.readCert(t, "/run/pki/apiserver.pem")
	oldAgent := r.readCert(t, "/run/pki/agent.pem")
	renamed := serverCert()
	renamed.CommonName = "renamed"
	spec := config.PKI{Authorities: r.spec.Authorities, Certificates: []config.Certificate{renamed, clientCert()}}

	require.NoError(t, r.issuer.Reissue(spec, []string{"apiserver"}))

	newServer := r.readCert(t, "/run/pki/apiserver.pem")
	assert.Equal(t, "renamed", newServer.Subject.CommonName)
	assert.NotEqual(t, oldServer.SerialNumber, newServer.SerialNumber)
	assert.True(t, r.readKey(t, "/run/pki/apiserver.key").PublicKey.Equal(newServer.PublicKey), "the pair matches")
	assert.Equal(t, oldAgent.SerialNumber, r.readCert(t, "/run/pki/agent.pem").SerialNumber, "a certificate that is not named is not issued again")
	r.restarts.none(t)
}

func TestReissue_CreatesACertificateTheSpecAddsAndRunRenewsIt(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	r.startRun(t)
	r.clock.nextTimer(t)
	spec := config.PKI{Authorities: r.spec.Authorities, Certificates: []config.Certificate{serverCert(), clientCert()}}

	require.NoError(t, r.issuer.Reissue(spec, []string{"agent"}))

	assert.FileExists(t, r.path("/run/pki/agent.pem"))
	oldAgent := r.readCert(t, "/run/pki/agent.pem")
	timer := r.clock.nextTimer(t)
	r.clock.advance(16 * time.Hour)
	timer.ch <- time.Time{}
	assert.Equal(t, "apiserver", r.restarts.next(t))
	assert.Equal(t, "agent", r.restarts.next(t))
	assert.NotEqual(t, oldAgent.SerialNumber, r.readCert(t, "/run/pki/agent.pem").SerialNumber, "the new certificate is renewed too")
}

func TestReissue_StopsRenewingACertificateTheSpecDropped(t *testing.T) {
	r := newRig(t, serverCert(), clientCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	agent := r.readCert(t, "/run/pki/agent.pem")
	require.NoError(t, r.issuer.Reissue(config.PKI{Authorities: r.spec.Authorities, Certificates: []config.Certificate{serverCert()}}, nil))
	r.startRun(t)

	r.clock.nextTimer(t) // set before Run took the Reissue's wake-up
	timer := r.clock.nextTimer(t)
	r.clock.advance(16 * time.Hour)
	timer.ch <- time.Time{}

	assert.Equal(t, "apiserver", r.restarts.next(t))
	r.clock.nextTimer(t)
	r.restarts.none(t)
	assert.Equal(t, agent.SerialNumber, r.readCert(t, "/run/pki/agent.pem").SerialNumber)
}

func TestReissue_RefusesACertificateItCannotSignAndKeepsTheOldPair(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	old := r.readCert(t, "/run/pki/apiserver.pem")
	bad := serverCert()
	bad.Authority = "nope"

	err := r.issuer.Reissue(config.PKI{Authorities: r.spec.Authorities, Certificates: []config.Certificate{bad}}, []string{"apiserver"})

	require.Error(t, err)
	assert.Equal(t, old.SerialNumber, r.readCert(t, "/run/pki/apiserver.pem").SerialNumber)
}

// A renewal and a Reissue of the same certificate at the same moment leave a
// pair that matches and the Reissue's new name, whichever runs first.
func TestReissue_AndARenewalAtTheSameTimeLoseNeitherWrite(t *testing.T) {
	for range 20 {
		r := newRig(t, serverCert())
		require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
		r.startRun(t)
		timer := r.clock.nextTimer(t)
		renamed := serverCert()
		renamed.CommonName = "renamed"
		spec := config.PKI{Authorities: r.spec.Authorities, Certificates: []config.Certificate{renamed}}
		r.clock.advance(16 * time.Hour)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, r.issuer.Reissue(spec, []string{"apiserver"}))
		}()
		timer.ch <- time.Time{}
		wg.Wait()
		// A renewal that raced the Reissue holds the Issuer's lock, so it has
		// finished when the Reissue returns, and one that has not started finds
		// the certificate not due.

		cert := r.readCert(t, "/run/pki/apiserver.pem")
		assert.Equal(t, "renamed", cert.Subject.CommonName, "the Reissue's certificate was not overwritten by an older one")
		assert.True(t, r.readKey(t, "/run/pki/apiserver.key").PublicKey.Equal(cert.PublicKey), "the pair is never torn")
	}
}
