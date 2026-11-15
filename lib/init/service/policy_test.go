package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/service"
)

func TestManager_OnFailureLeavesAServiceStoppedAfterExitZeroAndTellsWhoWaits(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(config.Service{Name: "join", Binary: "/bin/join", RestartPolicy: config.RestartOnFailure}))
	exited := r.manager.Exited("join")
	proc := recv(t, r.launcher.launched).proc
	select {
	case <-exited:
		t.Fatal("the channel is closed while the service runs")
	default:
	}

	proc.exit(nil)

	recv(t, exited)
	got := statusOf(t, r.manager, "join", func(s service.Status) bool { return s.State == service.StateExited })
	assert.Zero(t, got.PID)
	assert.Empty(t, r.clock.timers, "no restart is scheduled")
	assert.Empty(t, r.launcher.launched, "the service is not started again")
	kills, _ := r.cgroups.leaf("join").counts()
	assert.Equal(t, 1, kills, "what the process left is killed")

	r.manager.Stop("join")
	_, removes := r.cgroups.leaf("join").counts()
	assert.Equal(t, 1, removes, "a stopped service that exited still has its cgroup removed")
}

func TestManager_OnFailureRestartsAServiceThatFailsWithBackoff(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(config.Service{Name: "join", Binary: "/bin/join", RestartPolicy: config.RestartOnFailure}))
	exited := r.manager.Exited("join")

	recv(t, r.launcher.launched).proc.exit(errExit)

	tm := recv(t, r.clock.timers)
	assert.Equal(t, time.Second, tm.d)
	tm.fire()
	recv(t, r.launcher.launched)
	select {
	case <-exited:
		t.Fatal("a failed exit is not a clean one")
	default:
	}
}

func TestManager_AlwaysRestartsAServiceThatExitsZero(t *testing.T) {
	// The control of the two tests above: the default policy restarts after exit 0.
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(config.Service{Name: "join", Binary: "/bin/join"}))

	recv(t, r.launcher.launched).proc.exit(nil)

	recv(t, r.clock.timers).fire()
	recv(t, r.launcher.launched)
}

func TestManager_ExitedIsNilForAServiceThatIsNotRunning(t *testing.T) {
	r := newRig()

	assert.Nil(t, r.manager.Exited("nothing"))
}
