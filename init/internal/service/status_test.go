package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/service"
)

// statusOf waits until the status of name satisfies ok, and returns it.
func statusOf(t *testing.T, m service.Manager, name string, ok func(service.Status) bool) service.Status {
	t.Helper()
	var got service.Status
	require.Eventually(t, func() bool {
		for _, s := range m.Status() {
			if s.Name == name {
				got = s
				return ok(s)
			}
		}
		return false
	}, 5*time.Second, time.Millisecond)
	return got
}

func TestManager_StatusReportsARunningServiceWithItsPIDAndStartTime(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(mvmd))
	l := recv(t, r.launcher.launched)

	got := statusOf(t, r.manager, "mvmd", func(s service.Status) bool { return s.State == service.StateRunning })

	assert.Equal(t, service.Status{
		Name: "mvmd", State: service.StateRunning, PID: l.proc.pid, Restarts: 0, StartedAt: r.clock.Now(),
	}, got)
}

func TestManager_StatusReportsBackoffWithNoPIDAndCountsTheRestart(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(mvmd))
	first := recv(t, r.launcher.launched)
	first.proc.exit(errExit)
	tm := recv(t, r.clock.timers)

	backoff := statusOf(t, r.manager, "mvmd", func(s service.Status) bool { return s.State == service.StateBackoff && s.PID == 0 })
	tm.fire()
	second := recv(t, r.launcher.launched)
	running := statusOf(t, r.manager, "mvmd", func(s service.Status) bool { return s.State == service.StateRunning && s.Restarts == 1 })

	assert.Equal(t, 0, backoff.Restarts)
	assert.Equal(t, second.proc.pid, running.PID)
	assert.NotEqual(t, first.proc.pid, running.PID)
}

func TestManager_StatusListsServicesByNameAndDropsAStoppedOne(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(mvmd))
	require.NoError(t, r.manager.Start(config.Service{Name: "etcd", Binary: "/usr/bin/etcd"}))

	require.Eventually(t, func() bool { return len(r.manager.Status()) == 2 }, 5*time.Second, time.Millisecond)
	names := []string{r.manager.Status()[0].Name, r.manager.Status()[1].Name}
	r.manager.Stop("etcd")

	assert.Equal(t, []string{"etcd", "mvmd"}, names)
	assert.Len(t, r.manager.Status(), 1)
}
