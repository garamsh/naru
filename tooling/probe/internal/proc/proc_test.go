package proc

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stat is a /proc/PID/stat line with comm and the start time of the argument.
func stat(pid int, comm, state string, start uint64) string {
	return strconv.Itoa(pid) + " (" + comm + ") " + state +
		" 1 1 1 0 -1 4194560 100 0 0 0 1 1 0 0 20 0 1 0 " + strconv.FormatUint(start, 10) +
		" 1000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0\n"
}

// fake writes a /proc with the processes and a cgroup directory listing the pids.
func fake(t *testing.T, procs map[int]string, cgroupPids ...int) (Reader, string) {
	t.Helper()
	root := t.TempDir()
	for pid, line := range procs {
		dir := filepath.Join(root, "proc", strconv.Itoa(pid))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(line), 0o644))
	}
	cg := filepath.Join(root, "cg")
	require.NoError(t, os.MkdirAll(cg, 0o755))
	var list string
	for _, p := range cgroupPids {
		list += strconv.Itoa(p) + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(cg, "cgroup.procs"), []byte(list), 0o644))
	return Reader{Proc: filepath.Join(root, "proc")}, cg
}

func TestFind_ReturnsTheProcessCalledComm(t *testing.T) {
	r, cg := fake(t, map[int]string{
		10: stat(10, "mvmd-supervisor", "S", 500),
		11: stat(11, "firecracker", "S", 501),
	}, 10, 11)

	p, err := r.Find(cg, "mvmd-supervisor")

	require.NoError(t, err)
	assert.Equal(t, Process{PID: 10, Start: 500, Comm: "mvmd-supervisor"}, p)
}

func TestFind_ReadsACommWithSpacesAndParentheses(t *testing.T) {
	r, cg := fake(t, map[int]string{7: stat(7, "a b) (c", "S", 42)}, 7)

	p, err := r.Find(cg, "a b) (c")

	require.NoError(t, err)
	assert.Equal(t, uint64(42), p.Start)
}

func TestFind_FailsWithoutExactlyOneMatch(t *testing.T) {
	r, cg := fake(t, map[int]string{
		10: stat(10, "mvmd-supervisor", "S", 500),
		11: stat(11, "mvmd-supervisor", "S", 501),
	}, 10, 11)

	_, none := r.Find(cg, "absent")
	_, two := r.Find(cg, "mvmd-supervisor")

	assert.ErrorContains(t, none, "no process called")
	assert.ErrorContains(t, two, "2 processes called")
}

func TestMembers_SkipsAProcessThatEndedMeanwhile(t *testing.T) {
	r, cg := fake(t, map[int]string{10: stat(10, "x", "S", 1)}, 10, 99)

	got, err := r.Members(cg)

	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestAlive_TellsTheSameProcessFromAReusedPIDAndAZombie(t *testing.T) {
	was := Process{PID: 10, Start: 500, Comm: "mvmd-supervisor"}
	same, _ := fake(t, map[int]string{10: stat(10, "mvmd-supervisor", "S", 500)})
	reused, _ := fake(t, map[int]string{10: stat(10, "mvmd-supervisor", "S", 900)})
	zombie, _ := fake(t, map[int]string{10: stat(10, "mvmd-supervisor", "Z", 500)})
	gone, _ := fake(t, nil)

	assert.True(t, same.Alive(was))
	assert.False(t, reused.Alive(was), "the PID runs a later process")
	assert.False(t, zombie.Alive(was), "a zombie has ended")
	assert.False(t, gone.Alive(was))
}
