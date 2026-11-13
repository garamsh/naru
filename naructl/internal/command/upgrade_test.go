package command_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundleWithSums writes a bundle of body and a sums file that lists it with sum, which is the
// body's digest when it is empty.
func bundleWithSums(t *testing.T, body, sum string) (bundle, sums string) {
	t.Helper()
	dir := t.TempDir()
	bundle = filepath.Join(dir, "naru-upgrade-v2.tar")
	require.NoError(t, os.WriteFile(bundle, []byte(body), 0o644))
	if sum == "" {
		h := sha256.Sum256([]byte(body))
		sum = hex.EncodeToString(h[:])
	}
	sums = filepath.Join(dir, "SHA256SUMS")
	require.NoError(t, os.WriteFile(sums, []byte("0000000000000000000000000000000000000000000000000000000000000000  other.img\n"+sum+"  naru-upgrade-v2.tar\n"), 0o644))
	return bundle, sums
}

func TestUpgrade_SendsTheBundleWithTheDigestTheSumsFileListsAndPrintsTheActions(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	bundle, sums := bundleWithSums(t, "the bundle", "")

	out, err := run(t, "upgrade", "--config", node.clientFile(t, dep, "admin"), "--bundle", bundle, "--sums", sums)

	require.NoError(t, err, out)
	assert.Equal(t, []string{"POST /v1/upgrade"}, node.requests)
	assert.Equal(t, []byte("the bundle"), node.upgradeBody)
	h := sha256.Sum256([]byte("the bundle"))
	assert.Equal(t, "sha256="+hex.EncodeToString(h[:]), node.upgradeQuery)
	assert.Contains(t, out, "the Node accepted the upgrade:\n  upgrade v2\n  reboot node\n")
}

func TestUpgrade_RefusesABundleThatIsNotTheOneTheSumsFileListsBeforeAnyRequest(t *testing.T) {
	tests := map[string]struct {
		body, sum string
	}{
		"a digest of another file": {"the bundle", "1111111111111111111111111111111111111111111111111111111111111111"},
		"a digest that is short":   {"the bundle", "abcd"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dep := newDeployment(t)
			apiAuthority(t, dep)
			node := newFakeNode(t, dep)
			bundle, sums := bundleWithSums(t, tt.body, tt.sum)

			out, err := run(t, "upgrade", "--config", node.clientFile(t, dep, "admin"), "--bundle", bundle, "--sums", sums)

			require.Error(t, err)
			assert.Empty(t, node.requests, "nothing is sent: %s", out)
		})
	}
	t.Run("a bundle the sums file does not list", func(t *testing.T) {
		dep := newDeployment(t)
		apiAuthority(t, dep)
		node := newFakeNode(t, dep)
		bundle, sums := bundleWithSums(t, "the bundle", "")
		renamed := filepath.Join(filepath.Dir(bundle), "naru-upgrade-v3.tar")
		require.NoError(t, os.Rename(bundle, renamed))

		_, err := run(t, "upgrade", "--config", node.clientFile(t, dep, "admin"), "--bundle", renamed, "--sums", sums)

		require.Error(t, err)
		assert.Empty(t, node.requests)
	})
}

func TestUpgrade_AReaderIsRefusedWithTheNodesReason(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	bundle, sums := bundleWithSums(t, "the bundle", "")

	out, err := run(t, "upgrade", "--config", node.clientFile(t, dep, "reader"), "--bundle", bundle, "--sums", sums)

	require.Error(t, err)
	assert.Equal(t, "403 Forbidden: the client certificate's role does not allow this request", out)
}

func TestUpgrade_StatusPrintsTheSlotAndHowTheLastUpgradeEnded(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	file := node.clientFile(t, dep, "reader")

	out, err := run(t, "upgrade", "--status", "--config", file)
	require.NoError(t, err, out)
	assert.Contains(t, out, "slot     default")
	assert.Contains(t, out, "none has ended")

	node.lastUpgrade = map[string]any{"from": "v1", "to": "v2", "result": "rolled back", "reason": "the boot sequence refused", "time": "2026-11-13T01:02:03Z"}
	out, err = run(t, "upgrade", "--status", "--config", file)
	require.NoError(t, err, out)
	assert.Contains(t, out, "v1 -> v2, rolled back at 2026-11-13T01:02:03Z")
	assert.Contains(t, out, "reason   the boot sequence refused")
	assert.Equal(t, []string{"GET /v1/version", "GET /v1/version"}, node.requests, "status sends no upgrade")
}

func TestUpgrade_NeedsABundleWithItsSumsOrStatusAndNotBoth(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	file := node.clientFile(t, dep, "admin")
	bundle, sums := bundleWithSums(t, "the bundle", "")
	for name, args := range map[string][]string{
		"nothing":                 {"upgrade", "--config", file},
		"a bundle without sums":   {"upgrade", "--config", file, "--bundle", bundle},
		"sums without a bundle":   {"upgrade", "--config", file, "--sums", sums},
		"status with a bundle":    {"upgrade", "--config", file, "--status", "--bundle", bundle, "--sums", sums},
		"a bundle that is absent": {"upgrade", "--config", file, "--bundle", bundle + ".none", "--sums", sums},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(t, args...)
			assert.Error(t, err)
		})
	}
	assert.Empty(t, node.requests)
}
