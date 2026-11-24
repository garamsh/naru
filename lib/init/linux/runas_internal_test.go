package linux

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/lib/init/config"
)

// The capability is the kernel's to grant at exec, which needs root, so this white-box test reads
// what init asks the kernel for.
func TestProcAttr_AServiceThatBindsLowPortsAsAUserIsStartedWithThatOneCapability(t *testing.T) {
	user := &config.User{UID: 6003, GID: 6003}
	attr := procAttr(config.Service{User: user, BindLowPorts: true}, 7)
	assert.Equal(t, []uintptr{unix.CAP_NET_BIND_SERVICE}, attr.AmbientCaps)
	assert.Equal(t, uint32(6003), attr.Credential.Uid)
	assert.Equal(t, 7, attr.CgroupFD)

	assert.Empty(t, procAttr(config.Service{User: user}, 7).AmbientCaps, "control: a user's service without bindLowPorts gets none")
	assert.Empty(t, procAttr(config.Service{BindLowPorts: true}, 7).AmbientCaps, "root's service needs none")
}

// Raising a limit needs CAP_SYS_RESOURCE, so the test lowers a child's, which any user may.
func TestLimitOpenFiles_SetsTheStartedProcesssLimitSoftAndHard(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	var before unix.Rlimit
	require.NoError(t, unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_NOFILE, nil, &before))
	require.NoError(t, limitOpenFiles(cmd.Process.Pid, 0))
	var same unix.Rlimit
	require.NoError(t, unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_NOFILE, nil, &same))
	assert.Equal(t, before, same, "0 leaves the inherited limit")

	require.NoError(t, limitOpenFiles(cmd.Process.Pid, 64))
	var got unix.Rlimit
	require.NoError(t, unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_NOFILE, nil, &got))
	assert.Equal(t, unix.Rlimit{Cur: 64, Max: 64}, got)
}
