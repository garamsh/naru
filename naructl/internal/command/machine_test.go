package command_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
)

// twoMachines is a Machine bound to h-a and one moving from h-a to h-b.
func twoMachines() []orchapi.Machine {
	return []orchapi.Machine{
		{Caller: "garam", Machine: machineapi.Machine{Name: "web", ID: "0123456789abcdef", Status: machineapi.Status{Phase: machineapi.PhaseRunning}},
			Binding: &orchapi.MachineBinding{Host: "h-a", Epoch: 1}},
		{Caller: "garam", Machine: machineapi.Machine{Name: "db", ID: "fedcba9876543210", Status: machineapi.Status{Phase: machineapi.PhaseMoving}},
			Binding: &orchapi.MachineBinding{Host: "h-a", Epoch: 3}, Move: &orchapi.MachineMove{By: "drain", Step: orchapi.MoveStopped, Source: "h-a", Target: "h-b"}},
		{Caller: "functions", Machine: machineapi.Machine{Name: "job", ID: "00000000000000aa", Status: machineapi.Status{Phase: machineapi.PhasePending}}},
	}
}

func TestMachineList_ShowsEachMachinesHostAndMove(t *testing.T) {
	f := newFakeOrch(t)
	f.machines = twoMachines()
	op := newOperator(t, f, "reader")

	out, err := run(t, op.args(f, "machine", "list")...)
	require.NoError(t, err, out)
	assert.Regexp(t, `CALLER\s+NAME\s+PHASE\s+HOST\s+EPOCH\s+MOVE`, out)
	assert.Regexp(t, `garam\s+web\s+running\s+h-a\s+1\s+-\n`, out)
	assert.Regexp(t, `garam\s+db\s+moving\s+h-a\s+3\s+stopped h-a->h-b\n`, out)
	assert.Regexp(t, `functions\s+job\s+pending\s+-\s+-\s+-\n`, out, "a Machine on no host")
}

func TestMachineGet_PrintsTheMachine(t *testing.T) {
	f := newFakeOrch(t)
	f.machines = twoMachines()
	op := newOperator(t, f, "reader")

	out, err := run(t, op.args(f, "machine", "get", "garam/db")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"step": "stopped"`)
	assert.Contains(t, out, `"target": "h-b"`)

	_, err = run(t, op.args(f, "machine", "get", "garam/nope")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Machine garam/nope")
	_, err = run(t, op.args(f, "machine", "get", "garam")...)
	require.Error(t, err, "it needs the caller and the name")
	assert.Contains(t, err.Error(), `"garam" is not <caller>/<name>`)
}

func TestMachineMove_SendsTheNarrowingAndSaysWhatOrchRecorded(t *testing.T) {
	f := newFakeOrch(t)
	f.machines = twoMachines()
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "machine", "move", "garam/web")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "Machine garam/web moves from h-a, requested")
	out, err = run(t, op.args(f, "machine", "move", "garam/web", "--region", "kr-seoul", "--label", "disk=nvme", "--label", "zone=a")...)
	require.NoError(t, err, out)
	out, err = run(t, op.args(f, "machine", "move", "garam/db", "--cancel")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "the move of Machine garam/db is cancelled at stopped")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []sentMove{
		{call: "move garam/web"},
		{call: "move garam/web", req: orchapi.MoveRequest{Shape: &orchapi.MoveShape{Region: "kr-seoul", Labels: map[string]string{"disk": "nvme", "zone": "a"}}}},
		{call: "cancel garam/db"},
	}, f.moves)
}

func TestMachineMove_RefusesFlagsItCannotSend(t *testing.T) {
	f := newFakeOrch(t)
	f.machines = twoMachines()
	op := newOperator(t, f, "operator")

	_, err := run(t, op.args(f, "machine", "move", "garam/web", "--cancel", "--region", "kr-seoul")...)
	require.Error(t, err)
	_, err = run(t, op.args(f, "machine", "move", "garam/web", "--cancel", "--abandon-volumes")...)
	require.Error(t, err)
	_, err = run(t, op.args(f, "machine", "move", "garam/web", "--label", "disk")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `--label "disk" is not <key>=<value>`)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, f.moves, "nothing was sent")
}

func TestMachineMove_AbandonVolumesSaysTheOldOnesStayListed(t *testing.T) {
	f := newFakeOrch(t)
	f.machines = twoMachines()
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "machine", "move", "garam/web", "--abandon-volumes")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "Machine garam/web moves from h-a with new volumes, requested")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []sentMove{{call: "move garam/web", req: orchapi.MoveRequest{AbandonVolumes: true}}}, f.moves)
}

func TestAbandonedVolumes_ListsThemAndDeletesOneEpochs(t *testing.T) {
	f := newFakeOrch(t)
	f.abandoned = []orchapi.AbandonedVolumes{{Caller: "garam", Name: "web", ID: "0123456789abcdef", Host: "h-a", Epoch: 3, Volumes: []string{"data", "logs"}, At: time.Date(2026, 11, 28, 1, 2, 3, 0, time.UTC)}}
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "machine", "abandoned-volumes", "list")...)
	require.NoError(t, err, out)
	assert.Regexp(t, `CALLER\s+NAME\s+HOST\s+EPOCH\s+VOLUMES\s+AT`, out)
	assert.Regexp(t, `garam\s+web\s+h-a\s+3\s+data,logs\s+2026-11-28T01:02:03Z\n`, out)

	out, err = run(t, op.args(f, "machine", "abandoned-volumes", "delete", "garam/web", "--host", "h-a", "--epoch", "3")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "forgot the volumes of Machine garam/web epoch 3 on host h-a")
	_, err = run(t, op.args(f, "machine", "abandoned-volumes", "delete", "garam/web", "--host", "h-a")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--epoch is required")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []string{"garam/web/h-a/3"}, f.deletedVolumes)
}
