package service_test

import (
	"io"
	"log/slog"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/service"
)

type rig struct {
	clock    *fakeClock
	launcher *fakeLauncher
	cgroups  *fakeCgroups
	manager  service.Manager
}

func newRig() *rig {
	r := &rig{clock: newFakeClock(), launcher: newFakeLauncher(), cgroups: &fakeCgroups{}}
	r.manager = service.NewManager(r.cgroups, r.launcher, r.clock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return r
}

var mvmd = config.Service{Name: "mvmd", Binary: "/usr/bin/mvmd"}

func TestManager_StartsEachServiceInItsOwnCgroup(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)

	require.NoError(t, r.manager.Start(mvmd))
	require.NoError(t, r.manager.Start(config.Service{Name: "etcd", Binary: "/usr/bin/etcd"}))

	got := map[string]string{}
	for range 2 {
		l := recv(t, r.launcher.launched)
		got[l.svc.Name] = l.cgroup
	}
	assert.Equal(t, map[string]string{"mvmd": "/cgroup/naru/mvmd", "etcd": "/cgroup/naru/etcd"}, got)
}

func TestManager_RestartsAnExitedServiceWithBackoff(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(mvmd))
	leaf := r.cgroups.leaf("mvmd")

	// Consecutive quick failures double the delay up to the cap.
	var delays []time.Duration
	for range 7 {
		recv(t, r.launcher.launched).proc.exit(errExit)
		tm := recv(t, r.clock.timers)
		delays = append(delays, tm.d)
		tm.fire()
	}
	assert.Equal(t, []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second,
	}, delays)

	// A run of 60 s resets the delay.
	p := recv(t, r.launcher.launched).proc
	r.clock.advance(60 * time.Second)
	p.exit(errExit)
	tm := recv(t, r.clock.timers)
	assert.Equal(t, time.Second, tm.d)
	tm.fire()

	// A run just short of 60 s does not.
	p = recv(t, r.launcher.launched).proc
	r.clock.advance(59 * time.Second)
	p.exit(nil)
	tm = recv(t, r.clock.timers)
	assert.Equal(t, 2*time.Second, tm.d)

	kills, _ := leaf.counts()
	assert.Equal(t, 9, kills, "every exit kills what the process left in its cgroup")
}

func TestManager_RestartsAServiceThatFailsToStart(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	r.launcher.failures <- errExit
	require.NoError(t, r.manager.Start(mvmd))

	tm := recv(t, r.clock.timers)
	assert.Equal(t, time.Second, tm.d)
	tm.fire()

	assert.Equal(t, "mvmd", recv(t, r.launcher.launched).svc.Name)
}

func TestManager_StopSignalsTheServiceAndEmptiesItsCgroup(t *testing.T) {
	r := newRig()
	require.NoError(t, r.manager.Start(mvmd))
	l := recv(t, r.launcher.launched)

	r.manager.Stop("mvmd")

	assert.Equal(t, []syscall.Signal{syscall.SIGTERM}, l.proc.received())
	kills, removes := r.cgroups.leaf("mvmd").counts()
	assert.Equal(t, 1, kills)
	assert.Equal(t, 1, removes)
	assert.Empty(t, r.launcher.launched, "a stopped service is not restarted")
}

func TestManager_KillsTheCgroupOfAServiceThatIgnoresSIGTERM(t *testing.T) {
	r := newRig()
	r.launcher.exitOnTerm = false
	require.NoError(t, r.manager.Start(mvmd))
	l := recv(t, r.launcher.launched)

	stopped := make(chan struct{})
	go func() { r.manager.Stop("mvmd"); close(stopped) }()

	grace := recv(t, r.clock.timers)
	assert.Equal(t, 5*time.Second, grace.d)
	kills, _ := r.cgroups.leaf("mvmd").counts()
	assert.Zero(t, kills, "the cgroup is not killed before the grace period ends")

	grace.fire()
	recv(t, stopped)

	kills, removes := r.cgroups.leaf("mvmd").counts()
	assert.Equal(t, 1, kills)
	assert.Equal(t, 1, removes)
	assert.Equal(t, []syscall.Signal{syscall.SIGTERM}, l.proc.received())
}

func TestManager_StopDuringBackoffDoesNotRestart(t *testing.T) {
	r := newRig()
	require.NoError(t, r.manager.Start(mvmd))
	recv(t, r.launcher.launched).proc.exit(errExit)
	recv(t, r.clock.timers)

	r.manager.Stop("mvmd")

	assert.Empty(t, r.launcher.launched)
	_, removes := r.cgroups.leaf("mvmd").counts()
	assert.Equal(t, 1, removes)
}

func TestManager_RefusesToStartAServiceTwice(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	require.NoError(t, r.manager.Start(mvmd))

	assert.Error(t, r.manager.Start(mvmd))
}

func TestManager_RestartStopsTheServiceAndStartsItAgainWithTheSameConfiguration(t *testing.T) {
	r := newRig()
	t.Cleanup(r.manager.StopAll)
	svc := config.Service{Name: "mvmd", Binary: "/usr/bin/mvmd", Args: []string{"--x"}}
	require.NoError(t, r.manager.Start(svc))
	first := recv(t, r.launcher.launched)

	r.manager.Restart("mvmd")

	assert.Equal(t, []syscall.Signal{syscall.SIGTERM}, first.proc.received())
	second := recv(t, r.launcher.launched)
	assert.Equal(t, svc, second.svc)
	assert.NotSame(t, first.proc, second.proc)
}

func TestManager_RestartDoesNotStartAServiceThatIsNotRunning(t *testing.T) {
	// The control is the test above: a running service is started again.
	r := newRig()

	r.manager.Restart("mvmd")

	assert.Empty(t, r.launcher.launched)
}

func TestManager_StopAllStopsEveryService(t *testing.T) {
	r := newRig()
	require.NoError(t, r.manager.Start(mvmd))
	require.NoError(t, r.manager.Start(config.Service{Name: "etcd", Binary: "/usr/bin/etcd"}))
	recv(t, r.launcher.launched)
	recv(t, r.launcher.launched)

	r.manager.StopAll()

	for _, name := range []string{"mvmd", "etcd"} {
		_, removes := r.cgroups.leaf(name).counts()
		assert.Equal(t, 1, removes, name)
	}
}
