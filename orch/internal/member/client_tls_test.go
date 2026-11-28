package member_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func TestOrchsClientOfItsMemberPresentsThePairAsItsFilesStand(t *testing.T) {
	ca := orchtest.NewCA(t)
	files := ca.IssueFiles(t, t.TempDir(), "m1")
	conf, err := member.ClientTLS(member.Config{Cert: files.Cert, Key: files.Key, CA: files.CA})
	require.NoError(t, err)
	got, err := conf.GetClientCertificate(nil)
	require.NoError(t, err)
	first := got.Certificate[0]

	next := ca.IssueFiles(t, t.TempDir(), "m1")
	for from, to := range map[string]string{next.Key: files.Key, next.Cert: files.Cert} {
		b, err := os.ReadFile(from)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(to+".new", b, 0o600))
		require.NoError(t, os.Rename(to+".new", to))
	}
	got, err = conf.GetClientCertificate(nil)
	require.NoError(t, err)
	require.NotEqual(t, first, got.Certificate[0], "the renewed pair, from the next handshake on")
}
