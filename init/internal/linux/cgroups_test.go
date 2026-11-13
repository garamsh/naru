package linux_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/console"
	"github.com/garamsh/naru/init/internal/linux"
	"github.com/garamsh/naru/init/internal/service"
)

// CgroupRootEnv names the cgroup v2 directory the real-cgroup tests run in.
// The directory must be one the user may write to (a delegated cgroup) and
// must hold no process but the test: make test-cgroup provides one.
const cgroupRootEnv = "NARU_TEST_CGROUP_ROOT"

// TestCgroups_ProcessMovedOutOfAServiceSurvivesItsStopAndRestart is the
// real-cgroup test of the detach rule: init kills the processes in a
// service's cgroup, and only those. It is run by make test-cgroup, not by
// make test, because it needs a delegated cgroup.
func TestCgroups_ProcessMovedOutOfAServiceSurvivesItsStopAndRestart(t *testing.T) {
	root := os.Getenv(cgroupRootEnv)
	if root == "" {
		t.Skipf("%s is not set: run make test-cgroup", cgroupRootEnv)
	}

	// The root of the tree may not hold a process once it delegates controllers.
	runner := filepath.Join(root, "runner")
	require.NoError(t, os.MkdirAll(runner, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(runner, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644))

	tree := linux.NewCgroupTree(root)
	reaper := linux.NewReaper()
	ctx, cancel := context.WithCancel(context.Background())
	go reaper.Run(ctx)
	t.Cleanup(cancel)
	mgr := service.NewManager(tree, linux.NewLauncher(reaper, console.New(io.Discard, console.KeptBytes)), service.SystemClock{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, mgr.Prepare())

	detached := filepath.Join(root, "naru", config.DetachedName)
	t.Cleanup(func() {
		mgr.StopAll()
		_ = os.WriteFile(filepath.Join(detached, "cgroup.kill"), []byte("1"), 0o644)
	})

	dir := t.TempDir()
	moved, kept := filepath.Join(dir, "moved.pid"), filepath.Join(dir, "kept.pid")
	// The service leaves one process in its cgroup (kept) and moves one out (moved).
	script := `sleep 300 & echo $! > ` + kept + `
sleep 300 & echo $! > ` + moved + `.tmp
echo $! > ` + detached + `/cgroup.procs
mv ` + moved + `.tmp ` + moved + `
wait`
	svc := config.Service{Name: "svc", Binary: "/bin/sh", Args: []string{"-c", script}, Env: map[string]string{"PATH": "/usr/bin:/bin"}}

	var movedPids []int
	for round := 1; round <= 2; round++ {
		require.NoError(t, os.RemoveAll(moved))
		require.NoError(t, os.RemoveAll(kept))
		require.NoError(t, mgr.Start(svc))
		movedPid := waitPid(t, moved)
		keptPid := waitPid(t, kept)
		movedPids = append(movedPids, movedPid)

		// Setup: the moved process is in the detached cgroup and the other in the service's.
		assert.Contains(t, procs(t, detached), movedPid, "round %d: process is in the detached cgroup", round)
		assert.Contains(t, procs(t, filepath.Join(root, "naru", "svc")), keptPid, "round %d: control is in the service's cgroup", round)

		mgr.Stop("svc")

		assert.True(t, waitGone(keptPid), "round %d: a process left in the service's cgroup is killed", round)
		for _, p := range movedPids {
			assert.True(t, alive(p), "round %d: process %d moved out of the service survives", round, p)
		}
	}
}

func waitPid(t *testing.T, file string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(file); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.FailNow(t, "service did not write its pid file", file)
	return 0
}

func procs(t *testing.T, cgroup string) []int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cgroup, "cgroup.procs"))
	require.NoError(t, err)
	var out []int
	for _, f := range strings.Fields(string(b)) {
		pid, err := strconv.Atoi(f)
		require.NoError(t, err)
		out = append(out, pid)
	}
	return out
}

// alive reports whether pid is a process that has not exited; a zombie has.
func alive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	state := s[strings.LastIndex(s, ")")+2]
	return state != 'Z' && state != 'X'
}

func TestCgroupTree_PrepareEnablesTheControllersInTheDelegatedGroup(t *testing.T) {
	root := t.TempDir()

	require.NoError(t, linux.NewCgroupTree(root).Prepare())

	got, err := os.ReadFile(filepath.Join(root, "naru", config.DelegatedName, "cgroup.subtree_control"))
	require.NoError(t, err)
	assert.Equal(t, "+cpu +cpuset +hugetlb +memory +pids", string(got))
}

func waitGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
