package pki_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClockStepped_RenewsACertificateTheNewTimeFindsExpiredOrNotYetValid(t *testing.T) {
	tests := map[string]time.Duration{
		"a step forward past its expiry":      100 * 24 * time.Hour,
		"a step back to before its beginning": -12 * time.Hour,
	}
	for name, step := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, serverCert())
			require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
			old := r.readCert(t, "/run/pki/apiserver.pem")
			r.startRun(t)
			r.clock.nextTimer(t)

			r.clock.advance(step)
			r.issuer.ClockStepped()

			assert.Equal(t, "apiserver", r.restarts.next(t))
			renewed := r.readCert(t, "/run/pki/apiserver.pem")
			assert.NotEqual(t, old.SerialNumber, renewed.SerialNumber)
			now := epoch.Add(step)
			assert.True(t, renewed.NotAfter.Equal(now.Add(24*time.Hour)), "valid for its validity from the new time")
			assert.False(t, now.Before(renewed.NotBefore), "valid at the new time")
			assert.Contains(t, r.log.String(), "msg=\"certificate renewed\" name=apiserver")
		})
	}
}

func TestClockStepped_LeavesACertificateThatIsStillValidAndWorksOutWhenItIsDue(t *testing.T) {
	r := newRig(t, serverCert())
	require.NoError(t, r.issuer.Issue(r.spec, nodeAddrs))
	old := r.readCert(t, "/run/pki/apiserver.pem")
	r.startRun(t)
	r.clock.nextTimer(t)

	r.clock.advance(10 * time.Hour)
	r.issuer.ClockStepped()

	assert.Equal(t, 6*time.Hour, r.clock.nextTimer(t).d, "due at 16h of a clock that now reads 10h")
	r.restarts.none(t)
	assert.Equal(t, old.SerialNumber, r.readCert(t, "/run/pki/apiserver.pem").SerialNumber)
}
