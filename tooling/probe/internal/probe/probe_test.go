package probe

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/tooling/probe/internal/proc"
)

// node is a fake Node: mvmd and a supervisor, and a Machine whose state the
// test can bend.
type node struct {
	mvmd, supervisor proc.Process
	// supervisorAfter is the supervisor once mvmd has been killed.
	supervisorAfter proc.Process
	supervisorAlive bool
	// stateAfter is the state List reports after the kill.
	stateAfter string

	killed    bool
	restarted bool
	// looks counts the looks at mvmd since the kill; mvmd is back after three.
	looks int
	// stuck means mvmd is never restarted.
	stuck bool
	// pullFails is the number of pulls that fail before one succeeds.
	pullFails int
}

func newNode() *node {
	sup := proc.Process{PID: 20, Start: 7, Comm: "mvmd-supervisor"}
	return &node{
		mvmd:            proc.Process{PID: 10, Start: 5, Comm: "mvmd"},
		supervisor:      sup,
		supervisorAfter: sup,
		supervisorAlive: true,
		stateAfter:      "RUNNING",
	}
}

func (n *node) Ready(context.Context) error {
	if n.killed && !n.restarted {
		return errors.New("connection refused")
	}
	return nil
}
func (n *node) Arm(context.Context) error { return nil }
func (n *node) Pull(context.Context, string, string) error {
	if n.pullFails > 0 {
		n.pullFails--
		return errors.New("registry not up")
	}
	return nil
}
func (n *node) CreateAndStart(context.Context, string, string) error { return nil }
func (n *node) States(context.Context, string) (string, string, error) {
	if n.killed {
		return "RUNNING", n.stateAfter, nil
	}
	return "RUNNING", "RUNNING", nil
}

func (n *node) Find(_, comm string) (proc.Process, error) {
	switch comm {
	case "mvmd-supervisor":
		if n.killed {
			return n.supervisorAfter, nil
		}
		return n.supervisor, nil
	case "mvmd":
		if n.killed {
			n.looks++
		}
		if n.killed && n.looks > 3 && !n.stuck {
			n.restarted = true
			return proc.Process{PID: 11, Start: 9, Comm: "mvmd"}, nil
		}
		return n.mvmd, nil
	}
	return proc.Process{}, errors.New("no such process")
}
func (n *node) Alive(proc.Process) bool { return n.supervisorAlive }
func (n *node) Kill(int) error          { n.killed = true; return nil }

func runProbe(t *testing.T, n *node) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	ok := Run(ctx, Config{
		Repository: "r", Digest: "sha256:d", MachineID: "m",
		MvmdCgroup: "mvmd", SupervisorCgroup: "detached", Running: "RUNNING", Poll: time.Millisecond,
	}, n, n, &out)
	return out.String(), ok
}

func TestRun_PassesWhenTheMachineOutlivesMvmd(t *testing.T) {
	out, ok := runProbe(t, newNode())

	assert.True(t, ok, out)
	for _, want := range []string{
		"PROBE machine-running ok", "PROBE mvmd-killed ok", "PROBE mvmd-restarted ok old=10 new=11",
		"PROBE supervisor-alive ok", "PROBE supervisor-same-pid ok pid=20", "PROBE machine-still-running ok", "PROBE finished",
	} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, " fail ")
}

func TestRun_WaitsForTheRegistryToAnswerThePull(t *testing.T) {
	n := newNode()
	n.pullFails = 3

	out, ok := runProbe(t, n)

	assert.True(t, ok, out)
	assert.Contains(t, out, "PROBE image-pulled ok")
}

func TestRun_FailsWhenThePullNeverSucceeds(t *testing.T) {
	n := newNode()
	n.pullFails = 1 << 30

	out, ok := runProbe(t, n)

	assert.False(t, ok)
	assert.Contains(t, out, "PROBE image-pulled fail")
	assert.Contains(t, out, "registry not up")
}

func TestRun_FailsWhenTheSupervisorEnded(t *testing.T) {
	n := newNode()
	n.supervisorAlive = false

	out, ok := runProbe(t, n)

	assert.False(t, ok)
	assert.Contains(t, out, "PROBE supervisor-alive fail")
	assert.Contains(t, out, "PROBE finished", "a probe that ended says so")
	assert.NotContains(t, out, "machine-still-running")
}

func TestRun_FailsWhenTheSupervisorIsAnotherProcess(t *testing.T) {
	n := newNode()
	n.supervisorAfter = proc.Process{PID: 21, Start: 12, Comm: "mvmd-supervisor"}

	out, ok := runProbe(t, n)

	assert.False(t, ok)
	assert.Contains(t, out, "PROBE supervisor-same-pid fail the supervisor is pid 21, was 20")
}

func TestRun_FailsWhenMvmdDoesNotReportTheMachineRunning(t *testing.T) {
	n := newNode()
	n.stateAfter = "STOPPED"

	out, ok := runProbe(t, n)

	assert.False(t, ok)
	assert.Contains(t, out, "PROBE machine-still-running fail")
	assert.True(t, strings.Contains(out, "List says STOPPED"), out)
}

func TestRun_FailsWhenMvmdIsNeverRestarted(t *testing.T) {
	n := newNode()
	n.stuck = true

	out, ok := runProbe(t, n)

	assert.False(t, ok)
	assert.Contains(t, out, "PROBE mvmd-restarted fail")
	assert.Contains(t, out, "PROBE finished")
}
