package machines_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

func TestGuest_ReachesTheRunningMachinesMicroVMOnItsHost(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	m := r.create("web", spec())
	r.pass()
	require.Equal(t, machineapi.PhaseRunning, r.record("web").Status.Phase)
	ctx := t.Context()

	require.NoError(t, r.svc.WriteFile(ctx, "garam", "web", "/data/a", []byte("hello")))
	out, err := r.svc.Exec(ctx, "garam", "web", machineapi.ExecRequest{Command: []string{"cat", "/data/a"}, TimeoutSeconds: 30})
	require.NoError(t, err)
	assert.Equal(t, machineapi.ExecResult{Stdout: []byte("hello"), Stderr: []byte{}}, out)
	assert.Equal(t, []hostapi.ExecRequest{{Command: []string{"cat", "/data/a"}, TimeoutSeconds: 30}}, r.hosts.Execs("h-a")[:1], "the host gets the command and its timeout")
	out, err = r.svc.Exec(ctx, "garam", "web", machineapi.ExecRequest{Command: []string{"sleep"}, TimeoutSeconds: 1})
	require.NoError(t, err)
	assert.Equal(t, machineapi.ExecResult{ExitCode: 137, Signal: 9, TimedOut: true, Stdout: []byte{}, Stderr: []byte{}}, out, "a timeout is a result, with its signal")
	b, err := r.svc.ReadFile(ctx, "garam", "web", "/data/a")
	require.NoError(t, err)
	assert.Equal(t, "hello", string(b))
	_, err = r.svc.ReadFile(ctx, "garam", "web", "/data/b")
	assert.ErrorIs(t, err, machines.ErrFileNotFound)
	assert.Contains(t, r.hosts.MicroVMs("h-a"), "m-"+m.ID+"-1", "the microVM the calls reached")
}

func TestGuest_RefusesAMachineThatDoesNotRun(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	ctx := t.Context()
	r.create("web", spec())
	exec := machineapi.ExecRequest{Command: []string{"cat"}}
	_, err := r.svc.Exec(ctx, "garam", "web", exec)
	assert.ErrorIs(t, err, machines.ErrNotRunning, "a Machine not yet placed")

	r.pass()
	_, err = r.svc.Exec(ctx, "garam", "web", exec)
	require.NoError(t, err, "control: the running Machine")
	_, err = r.svc.Exec(ctx, "functions", "web", exec)
	assert.ErrorIs(t, err, machines.ErrNotFound, "another caller's Machine")

	_, err = r.svc.Stop(ctx, "garam", "web", machineapi.StopRequest{})
	require.NoError(t, err)
	for what, call := range map[string]func() error{
		"exec":       func() error { _, err := r.svc.Exec(ctx, "garam", "web", exec); return err },
		"file read":  func() error { _, err := r.svc.ReadFile(ctx, "garam", "web", "/data/a"); return err },
		"file write": func() error { return r.svc.WriteFile(ctx, "garam", "web", "/data/a", nil) },
	} {
		assert.ErrorIs(t, call(), machines.ErrNotRunning, "a Machine stopping: "+what)
	}
}

func TestGuest_RefusesWhatItsBoundsDoNotTakeBeforeTheHost(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.create("web", spec())
	r.pass()
	ctx := t.Context()

	_, err := r.svc.Exec(ctx, "garam", "web", machineapi.ExecRequest{Command: []string{"cat"}, TimeoutSeconds: machineapi.MaxExecSeconds + 1})
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "a timeout above the most")
	_, err = r.svc.Exec(ctx, "garam", "web", machineapi.ExecRequest{})
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "no command")
	_, err = r.svc.ReadFile(ctx, "garam", "web", "data/a")
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "a path that is not absolute")
	assert.ErrorIs(t, r.svc.WriteFile(ctx, "garam", "web", "/data/a", make([]byte, machineapi.MaxGuestBytes+1)), machines.ErrTooLarge)
	assert.Empty(t, r.hosts.Execs("h-a"), "no refused exec reached the host")
	_, err = r.svc.ReadFile(ctx, "garam", "web", "/data/a")
	assert.ErrorIs(t, err, machines.ErrFileNotFound, "no refused write reached the host")

	_, err = r.svc.Exec(ctx, "garam", "web", machineapi.ExecRequest{Command: []string{"cat"}, TimeoutSeconds: machineapi.MaxExecSeconds})
	assert.NoError(t, err, "control: the most an exec runs")
	assert.NoError(t, r.svc.WriteFile(ctx, "garam", "web", "/data/a", make([]byte, machineapi.MaxGuestBytes)), "control: the most a file holds")
}

func TestGuest_IsUnavailableOnAMemberThatReachesNoHost(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.create("web", spec())
	r.pass()
	svc := machines.NewService(r.store, r.clock, "m.example.test", nil)
	_, err := svc.Exec(t.Context(), "garam", "web", machineapi.ExecRequest{Command: []string{"cat"}})
	assert.ErrorIs(t, err, machines.ErrHostUnavailable)
	_, err = r.svc.Exec(t.Context(), "garam", "web", machineapi.ExecRequest{Command: []string{"cat"}})
	assert.NoError(t, err, "control: the member that reaches the hosts")
}

func TestGuestBounds_AreTheHostAPIs(t *testing.T) {
	assert.Equal(t, hostapi.MaxExecSeconds, machineapi.MaxExecSeconds)
	assert.Equal(t, hostapi.MaxGuestBytes, machineapi.MaxGuestBytes)
	for _, p := range []string{"/data/a", "data/a", "/data/../a", "/", "/proc", "/proc/1", "/sys/x", "/dev/null", "/run/mvmd/x", "/run/other", "/data/"} {
		assert.Equal(t, hostapi.CheckGuestPath(p) == nil, machineapi.CheckPath(p) == nil, p)
	}
}
