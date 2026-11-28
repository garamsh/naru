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

	_, err = newBootSets(release.Config{Location: dir, Issuer: release.DefaultIssuer})
	require.Error(t, err, "an identity set to empty is an error at start")

	for _, identity := range []string{release.DefaultIdentity, "https://github.com/garamsh/naru/*"} {
		store, err = newBootSets(release.Config{Location: dir, Issuer: release.DefaultIssuer, Identity: identity})
		require.NoError(t, err, "control: %q", identity)
		require.NotNil(t, store)
	}
}

func TestOrchPresentsItsRenewedPairToTheHostsUnlessTheOperatorNamesAnother(t *testing.T) {
	f := runFlags{cert: "/run/naru-orchd/orch.crt", key: "/run/naru-orchd/orch.key", renewedCert: "/var/lib/naru/orch/renewed/orch.crt"}
	for _, given := range []runFlags{f, {cert: f.cert, key: f.key, renewedCert: f.renewedCert, hostCert: f.cert, hostKey: f.key}} {
		cert, key := hostPair(given)
		require.Equal(t, f.renewedCert, cert, "absent, or --cert and --key as init gives them")
		require.Equal(t, f.key, key)
	}
	other := f
	other.hostCert, other.hostKey = "/etc/other.crt", "/etc/other.key"
	cert, key := hostPair(other)
	require.Equal(t, "/etc/other.crt", cert, "control: the operator's own pair")
	require.Equal(t, "/etc/other.key", key)
}
