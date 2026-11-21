package member_test

import (
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestStart_RunsEtcdWithPeriodicCompactionABoundedQuotaAndFewFiles(t *testing.T) {
	ca := orchtest.NewCA(t)
	dir := t.TempDir()
	files := ca.IssueFiles(t, dir, "m1")
	peer, client := fmt.Sprintf("https://127.0.0.1:%d", freePort(t)), fmt.Sprintf("https://127.0.0.1:%d", freePort(t))

	m, err := member.Start(t.Context(), member.Config{
		Name: "m1", DataDir: dir + "/etcd", PeerURL: peer, ClientURL: client, InitialCluster: "m1=" + peer, State: member.StateNew,
		Cert: files.Cert, Key: files.Key, CA: files.CA,
	})
	require.NoError(t, err)
	t.Cleanup(m.Close)

	cfg := m.EtcdConfig()
	assert.Equal(t, "periodic", cfg.AutoCompactionMode)
	assert.Equal(t, member.CompactionRetention, cfg.AutoCompactionRetention)
	disk, err := member.DiskBytes(dir)
	require.NoError(t, err)
	assert.Equal(t, member.Quota(disk), cfg.QuotaBackendBytes, "the quota is a share of the disk, not etcd's default")
	assert.Equal(t, uint(2), cfg.MaxWalFiles)
	assert.Equal(t, uint(2), cfg.MaxSnapFiles)
}
