package pki_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/naructl/internal/pki"
)

func TestNewToken_IsInTheShapeGaonReadsAndDiffersEachTime(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		tok, err := pki.NewToken()
		require.NoError(t, err)
		assert.Regexp(t, `^[a-z0-9]{6}\.[a-z0-9]{16}$`, tok)
		assert.False(t, seen[tok], "a token is not drawn twice")
		seen[tok] = true
	}
}

func TestValidToken_RefusesWhatGaonWouldRefuse(t *testing.T) {
	for _, bad := range []string{"", "abcdef", "abcde.0123456789abcdef", "abcdef.0123456789abcde", "ABCDEF.0123456789abcdef", "abcdef.0123456789abcdefg", "abcdef.0123456789abcdef\n"} {
		assert.False(t, pki.ValidToken(bad), "%q", bad)
	}
	assert.True(t, pki.ValidToken("abc123.0123456789abcdef"))
}

func TestEnsureToken_KeepsTheTokenItWroteAndWritesItPrivately(t *testing.T) {
	dir := t.TempDir()
	first, err := pki.EnsureToken(dir, "node-1")
	require.NoError(t, err)
	again, err := pki.EnsureToken(dir, "node-1")
	require.NoError(t, err)
	assert.Equal(t, first, again, "a second call returns the same token")
	st, err := os.Stat(filepath.Join(dir, pki.TokensDir, "node-1"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

func TestEnsureToken_RefusesANameThatIsNotADNSLabelAndAFileThatIsNotAToken(t *testing.T) {
	dir := t.TempDir()
	_, err := pki.EnsureToken(dir, "../x")
	assert.ErrorContains(t, err, "not a DNS label")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, pki.TokensDir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, pki.TokensDir, "node-2"), []byte("hello\n"), 0o600))
	_, err = pki.EnsureToken(dir, "node-2")
	assert.ErrorContains(t, err, "does not hold a bootstrap token")
}

func TestReadTokens_ListsEveryNodesTokenInTheOrderOfTheNames(t *testing.T) {
	dir := t.TempDir()
	var want []string
	for _, n := range []string{"node-1", "node-2", "node-3"} {
		tok, err := pki.EnsureToken(dir, n)
		require.NoError(t, err)
		want = append(want, tok)
	}
	got, err := pki.ReadTokens(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
