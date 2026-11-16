package run

import (
	"errors"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEarlyBoot_RefusesAtTheFirstStepThatFails(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name       string
		leave, pse error
		wantMount  bool
		want       string
	}{
		{"leaving the rootfs fails", boom, nil, false, "leave the rootfs: boom"},
		{"mounting the pseudo file systems fails", nil, boom, true, "mount pseudo file systems: boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mounted := false

			err := earlyBoot(func() error { return tt.leave }, func() error { mounted = true; return tt.pse })

			require.Error(t, err)
			assert.ErrorIs(t, err, boom)
			assert.EqualError(t, err, tt.want)
			assert.Equal(t, tt.wantMount, mounted, "no step runs after the one that failed")
		})
	}
}

func TestEarlyBoot_PassesWhenBothStepsDo(t *testing.T) {
	assert.NoError(t, earlyBoot(func() error { return nil }, func() error { return nil }))
}

// A SIGHUP or SIGQUIT sent to a Go program that does not ignore it ends the program, which for PID 1 is a kernel panic.
func TestIgnoreSignals_InitSurvivesTheSignalsItDoesNotActOn(t *testing.T) {
	ignoreSignals()

	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGABRT} {
		require.NoError(t, syscall.Kill(syscall.Getpid(), sig))
	}
	// The test is still running: none of them ended the process.
}
