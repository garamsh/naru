package linux

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

func TestHost_SettleUnmountsExtensionsThenStorageThenTheDataDiskAndLogsEachFailure(t *testing.T) {
	var order []string
	h := &Host{
		dataMountPoint: "/var/lib/naru",
		storageMounts:  []string{"/storage/a", "/storage/b"},
		unmount: func(dir string) error {
			order = append(order, dir)
			if dir == "/storage/b" {
				return errors.New("busy")
			}
			return nil
		},
	}
	var logs bytes.Buffer

	h.Settle(slog.New(slog.NewTextHandler(&logs, nil)), []string{"/ext/one", "/ext/two"})

	assert.Equal(t, []string{"/ext/one", "/ext/two", "/storage/b", "/storage/a", "/var/lib/naru"}, order,
		"extensions, storage disks last mounted first, then the data disk; a failure does not stop the rest")
	assert.Contains(t, logs.String(), `msg="unmount at shutdown failed" kind="storage disk" mount=/storage/b err=busy`)
	assert.Contains(t, logs.String(), `msg="unmounted at shutdown" kind="data disk" mount=/var/lib/naru`)
}

func TestCgroupTree_KillDetachedKillsTheDetachedGroup(t *testing.T) {
	root := t.TempDir()
	detached := filepath.Join(root, naruGroup, config.DetachedName)
	require.NoError(t, os.MkdirAll(detached, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(detached, "cgroup.kill"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(detached, "cgroup.procs"), nil, 0o644))

	require.NoError(t, NewCgroupTree(root).KillDetached())

	killed, err := os.ReadFile(filepath.Join(detached, "cgroup.kill"))
	require.NoError(t, err)
	assert.Equal(t, "1", string(killed))
}
