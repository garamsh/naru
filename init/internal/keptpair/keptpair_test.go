package keptpair_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/keptpair"
	"github.com/garamsh/naru/init/internal/pkitest"
)

func pairFile(t *testing.T, orgs ...string) (config.KeptPair, string) {
	t.Helper()
	ca := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	cert, key := pkitest.Issue(t, ca, orgs...)
	body := cert + key
	file := filepath.Join(t.TempDir(), "node-agent.pem")
	require.NoError(t, os.WriteFile(file, []byte(body), 0o600))
	return config.KeptPair{File: file, Organization: "gaon:nodes"}, body
}

func TestSetAside_RenamesAPairWhoseSubjectLacksTheOrganization(t *testing.T) {
	p, body := pairFile(t)

	moved, err := keptpair.SetAside(p)

	require.NoError(t, err)
	assert.True(t, moved)
	assert.NoFileExists(t, p.File)
	got, err := os.ReadFile(p.File + keptpair.Suffix)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
}

func TestSetAside_KeepsAPairWithTheOrganization(t *testing.T) {
	p, _ := pairFile(t, "other", "gaon:nodes")

	moved, err := keptpair.SetAside(p)

	require.NoError(t, err)
	assert.False(t, moved)
	assert.FileExists(t, p.File)
	assert.NoFileExists(t, p.File+keptpair.Suffix)
}

func TestSetAside_LeavesAMissingOrUnreadableFileAlone(t *testing.T) {
	dir := t.TempDir()
	moved, err := keptpair.SetAside(config.KeptPair{File: filepath.Join(dir, "none.pem"), Organization: "o"})
	require.NoError(t, err)
	assert.False(t, moved)

	junk := filepath.Join(dir, "junk.pem")
	require.NoError(t, os.WriteFile(junk, []byte("not pem"), 0o600))
	moved, err = keptpair.SetAside(config.KeptPair{File: junk, Organization: "o"})
	require.NoError(t, err)
	assert.False(t, moved)
	assert.FileExists(t, junk)
}

func TestSetAside_ReplacesAnEarlierSetAsidePair(t *testing.T) {
	p, body := pairFile(t)
	require.NoError(t, os.WriteFile(p.File+keptpair.Suffix, []byte("older"), 0o600))

	moved, err := keptpair.SetAside(p)

	require.NoError(t, err)
	assert.True(t, moved)
	got, _ := os.ReadFile(p.File + keptpair.Suffix)
	assert.Equal(t, body, string(got), "one set-aside pair is kept, the newest")
}
