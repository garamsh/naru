//go:build qemu_real

package tests_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// microVMState is the state `naructl microvm get` reads for name.
func microVMState(t *testing.T, file, name string) string {
	t.Helper()
	r := hctl(file, []string{"microvm", "get"}, name)
	if r.Err != nil {
		return ""
	}
	var vm struct {
		State string `json:"state"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.Stdout), &vm), r.Stdout)
	return vm.State
}

// requestFile writes the image-form request for name, with restartOnBoot as given, for -f.
func requestFile(t *testing.T, name string, restartOnBoot bool) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": name, "image": inputs.Image.Repository + "@" + inputs.Image.Digest,
		"cpu": 1, "memory": 256 << 20, "rootDiskBytes": 1 << 30, "restartOnBoot": restartOnBoot,
	})
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), name+".json")
	require.NoError(t, os.WriteFile(p, body, 0o600))
	return p
}

func TestNode_HostAPIStopsAndStartsAMicroVMAndMvmdListsItStoppedThenRunning(t *testing.T) {
	c := hostCluster(t)
	nd := c.Nodes[0]
	prov, reader := c.Host.Provisioner, c.Host.Reader
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "started", "service=naru-hostd"))
	made := hctl(prov, []string{"microvm", "create"}, "-f", requestFile(t, "web", false))
	require.NoError(t, made.Err, made.refusal())
	eventually(t, "mvmd lists the Machine Running", func() bool { return strings.Contains(machines(nd), "web=RUNNING") })

	denied := hctl(reader, []string{"microvm", "stop"}, "web")
	require.Error(t, denied.Err)
	assert.Contains(t, denied.refusal(), "403")

	stopped := hctl(prov, []string{"microvm", "stop"}, "web")
	require.NoError(t, stopped.Err, stopped.refusal())
	eventually(t, "mvmd lists the Machine Stopped", func() bool { return strings.Contains(machines(nd), "web=STOPPED") })
	assert.Equal(t, "stopped", microVMState(t, reader, "web"))
	again := hctl(prov, []string{"microvm", "stop"}, "web")
	assert.NoError(t, again.Err, "a second stop is 200: %s", again.refusal())

	started := hctl(prov, []string{"microvm", "start"}, "web")
	require.NoError(t, started.Err, started.refusal())
	eventually(t, "mvmd lists the Machine Running again", func() bool { return strings.Contains(machines(nd), "web=RUNNING") })
	assert.Equal(t, "running", microVMState(t, reader, "web"))
	assert.NoError(t, hctl(prov, []string{"microvm", "start"}, "web").Err, "a second start is 200")

	deleted := hctl(prov, []string{"microvm", "delete"}, "web")
	require.NoError(t, deleted.Err, deleted.refusal())
	eventually(t, "mvmd no longer lists the Machine", func() bool { return machines(nd) == "none" })
}

func TestNode_ARestartOnBootMicroVMIsRunningAgainAfterTheHostRebootsAndAStoppedOneIsNot(t *testing.T) {
	c := hostCluster(t)
	nd := c.Nodes[0]
	prov, reader := c.Host.Provisioner, c.Host.Reader
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "started", "service=naru-hostd"))
	for name, restart := range map[string]bool{"up": true, "down": true, "plain": false} {
		made := hctl(prov, []string{"microvm", "create"}, "-f", requestFile(t, name, restart))
		require.NoError(t, made.Err, made.refusal())
	}
	eventually(t, "the three run", func() bool {
		m := machines(nd)
		return strings.Contains(m, "up=RUNNING") && strings.Contains(m, "down=RUNNING") && strings.Contains(m, "plain=RUNNING")
	})
	require.NoError(t, hctl(prov, []string{"microvm", "stop"}, "down").Err)
	eventually(t, "down is Stopped", func() bool { return microVMState(t, reader, "down") == "stopped" })

	r := ctl(c.API.Admin, "reboot")
	require.NoError(t, r.Err, r.refusal())
	began := time.Now()
	awaitRebootAndRestart(t, nd, c.API.Admin)
	eventually(t, "the host API answers again", func() bool { return hctl(reader, []string{"host"}).Err == nil })
	eventually(t, "up is running again", func() bool { return microVMState(t, reader, "up") == "running" })
	back := time.Since(began)
	t.Logf("restart-on-boot: the microVM was running again %s after the reboot was requested", back.Round(time.Second))

	assert.Equal(t, "stopped", microVMState(t, reader, "down"), "a stopped microVM is not restarted")
	assert.NotEqual(t, "running", microVMState(t, reader, "plain"), "restartOnBoot false: not restarted")
}
