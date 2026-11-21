package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups/release"
)

func TestNewBootSets(t *testing.T) {
	store, err := newBootSets(release.Config{})
	require.NoError(t, err)
	require.Nil(t, store, "no --boot-set-store: the feature is off")

	dir := t.TempDir()
	_, err = newBootSets(release.Config{Location: dir, Issuer: release.DefaultIssuer, Identity: "https://github.com/garamsh/naru*"})
	require.Error(t, err, "a prefix that is not under one repository is an error at start")

	for _, identity := range []string{"", "https://github.com/garamsh/naru/*"} {
		store, err = newBootSets(release.Config{Location: dir, Issuer: release.DefaultIssuer, Identity: identity})
		require.NoError(t, err, "control: %q", identity)
		require.NotNil(t, store)
	}
}
