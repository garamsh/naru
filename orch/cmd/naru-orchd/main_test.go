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
	_, err = newBootSets(release.Config{Location: dir, Issuer: release.DefaultIssuer})
	require.Error(t, err, "a store with no identity to verify against is an error at start")

	store, err = newBootSets(release.Config{Location: dir, Issuer: release.DefaultIssuer, Identity: "https://github.com/garamsh/naru/*"})
	require.NoError(t, err)
	require.NotNil(t, store)
}
