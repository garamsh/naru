package signer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/signer"
)

func TestEnsureIdentityRequiresTheFilesRootPlaced(t *testing.T) {
	dir := t.TempDir()
	f := signer.Files{Cert: filepath.Join(dir, "orch.crt"), Key: filepath.Join(dir, "orch.key"), CA: filepath.Join(dir, "ca.crt")}
	c := signer.New(filepath.Join(dir, "no.sock"))

	require.Error(t, c.EnsureIdentity(t.Context(), f, "orch", []string{"orch.example"}), "nothing is placed")
	for _, p := range []string{f.Cert, f.Key} {
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
	}
	require.Error(t, c.EnsureIdentity(t.Context(), f, "orch", nil), "the authority is missing")
	require.NoError(t, os.WriteFile(f.CA, []byte("x"), 0o600))
	require.NoError(t, c.EnsureIdentity(t.Context(), f, "orch", nil))
}
