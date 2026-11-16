package boot_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/boot"
	"github.com/garamsh/naru/lib/init/config"
)

const (
	maintSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	maintKey = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n-----END PUBLIC KEY-----\n"
	joinSeed = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
spec:
  join:
    orch:
      url: https://orch.example.test:7500
      caSHA256: ` + maintSHA + `
    enrollKey: naru-ek-fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210
`
	// brokenSeed is refused before the network comes up, and still names orch.
	brokenSeed = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
spec:
  bogus: 1
  join:
    orch:
      url: https://orch.example.test:7500
      caSHA256: ` + maintSHA + `
`
)

func anchor(t *testing.T, a boot.Anchor) []byte {
	t.Helper()
	b, err := json.Marshal(a)
	require.NoError(t, err)
	return b
}

func hostd(t *testing.T, r *recorder) config.Service {
	t.Helper()
	require.Len(t, r.startedSvc, 1, "calls: %v", r.calls)
	assert.Equal(t, "naru-hostd", r.startedSvc[0].Name)
	return r.startedSvc[0]
}

func TestRun_ARefusedSeedlessHostThatKeptAnAnchorServesMaintenanceOnItsStoredNetwork(t *testing.T) {
	r := seedless(t)
	r.stored = nil // the store was reset: there is no configuration
	r.anchor, r.hasAnchor = anchor(t, boot.Anchor{OrchURL: "https://orch.example.test:7500", OrchCASHA256: maintSHA, RevocationPublicKey: maintKey}), true

	_, err := runStored(r)

	require.Error(t, err, "the refusal itself is unchanged")
	svc := hostd(t, r)
	assert.Equal(t, "/bin/naru-hostd", svc.Binary)
	assert.Contains(t, svc.Args, "--maintenance")
	assert.Contains(t, svc.Args, "--orch-url=https://orch.example.test:7500")
	assert.Contains(t, svc.Args, "--orch-ca-sha256="+maintSHA)
	assert.Contains(t, svc.Args, "--revocation-public-key=/run/naru-hostd/maintenance-revocation.pub")
	require.Len(t, r.wrote, 1)
	assert.Equal(t, "/run/naru-hostd/maintenance-revocation.pub", r.wrote[0].Path)
	assert.Equal(t, maintKey, r.wrote[0].Content)
	assert.Empty(t, r.api, "no Node API: only hostd's maintenance mode is served")
}

func TestRun_AHostWithNoAnchorIsRefusedWithNoMaintenanceMode(t *testing.T) {
	r := seedless(t)
	r.stored = nil

	_, err := runStored(r)

	require.Error(t, err)
	assert.Empty(t, r.started)
}

func TestRun_NoMaintenanceModeWithoutARevocationKeyToCheckOperatorsAgainst(t *testing.T) {
	r := seedless(t)
	r.stored = nil
	r.anchor, r.hasAnchor = anchor(t, boot.Anchor{OrchURL: "https://orch.example.test:7500", OrchCASHA256: maintSHA}), true

	_, err := runStored(r)

	require.Error(t, err)
	assert.Empty(t, r.started)
}

func TestRun_TheSeedsFingerprintIsTheOneTrustedNotAStoredOne(t *testing.T) {
	r := &recorder{raw: brokenSeed}
	other := strings.Repeat("ab", 32)
	r.anchor, r.hasAnchor = anchor(t, boot.Anchor{OrchURL: "https://evil.example.test:1", OrchCASHA256: other, RevocationPublicKey: maintKey}), true

	_, err := runStored(r)

	require.Error(t, err)
	assert.Empty(t, r.started, "a stored key is not taken under another fingerprint: the seed's anchor has none")
}

func TestRun_ASeedRefusedBeforeTheNetworkIsBroughtUpOnTheNetworkConfigItNames(t *testing.T) {
	r := &recorder{raw: brokenSeed}
	r.anchor, r.hasAnchor = anchor(t, boot.Anchor{OrchURL: "https://orch.example.test:7500", OrchCASHA256: maintSHA, RevocationPublicKey: maintKey}), true

	_, err := runStored(r)

	require.Error(t, err)
	svc := hostd(t, r)
	assert.Contains(t, svc.Args, "--orch-ca-sha256="+maintSHA, "the fingerprint is the seed's")
	require.Len(t, r.appliedAll, 1)
	assert.Equal(t, "eth0", r.appliedAll[0].Interfaces[0].Name, "the seed's own network-config, which the refused boot did not apply")
}

func TestRun_AHostWhoseNetworkConfigIsUnusableGetsDHCPInMaintenance(t *testing.T) {
	r := &recorder{raw: brokenSeed, network: "not: [a network"}
	r.anchor, r.hasAnchor = anchor(t, boot.Anchor{OrchURL: "https://orch.example.test:7500", OrchCASHA256: maintSHA, RevocationPublicKey: maintKey}), true

	_, err := runStored(r)

	require.Error(t, err)
	hostd(t, r)
	require.Len(t, r.appliedAll, 1)
	require.Len(t, r.appliedAll[0].Interfaces, 1)
	assert.True(t, r.appliedAll[0].Interfaces[0].DHCP)
}

func TestRun_NoSecondHostdIsStartedInMaintenanceWhenHostdWasStartedAndTheBootRefused(t *testing.T) {
	r := &recorder{raw: joinSeed, startErr: errors.New("no")}
	r.anchor, r.hasAnchor = anchor(t, boot.Anchor{OrchURL: "https://orch.example.test:7500", OrchCASHA256: maintSHA, RevocationPublicKey: maintKey}), true

	_, err := runStored(r)

	require.Error(t, err)
	assert.Equal(t, []string{"naru-hostd"}, r.started, "started once, by the boot")
}

func TestRun_ABootThatSucceedsStoresTheAnchorOfItsOrch(t *testing.T) {
	r := &recorder{raw: joinSeed}

	_, err := runStored(r)

	require.NoError(t, err)
	var got boot.Anchor
	require.NoError(t, json.Unmarshal(r.anchorSaved, &got))
	assert.Equal(t, boot.Anchor{OrchURL: "https://orch.example.test:7500", OrchCASHA256: maintSHA}, got)
	// The control: a Node that names no orch stores none.
	none := &recorder{raw: validConfig}
	_, err = runStored(none)
	require.NoError(t, err)
	assert.Nil(t, none.anchorSaved)
}
