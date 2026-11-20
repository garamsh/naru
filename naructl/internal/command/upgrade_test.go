package command_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundleTar is a bundle for version: the kernel member and manifest.json.
func bundleTar(t *testing.T, kernel, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, m := range []struct{ name, body string }{{"kernel.efi", kernel}, {"manifest.json", `{"version":"` + version + `"}`}} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(m.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// bundleWithSums writes a bundle of the kernel body for v2 and a sums file that lists it with
// sum, which is the bundle's digest when it is empty.
func bundleWithSums(t *testing.T, body, sum string) (bundle, sums string) {
	t.Helper()
	dir := t.TempDir()
	bundle = filepath.Join(dir, "naru-host-upgrade-v2.tar")
	data := bundleTar(t, body, "v2")
	require.NoError(t, os.WriteFile(bundle, data, 0o644))
	if sum == "" {
		h := sha256.Sum256(data)
		sum = hex.EncodeToString(h[:])
	}
	sums = filepath.Join(dir, "SHA256SUMS")
	require.NoError(t, os.WriteFile(sums, []byte("0000000000000000000000000000000000000000000000000000000000000000  other.img\n"+sum+"  naru-host-upgrade-v2.tar\n"), 0o644))
	return bundle, sums
}

func TestUpgrade_SendsTheBundleWithTheDigestTheSumsFileListsAndPrintsTheActions(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	bundle, sums := bundleWithSums(t, "the bundle", "")

	out, err := run(t, "upgrade", "--config", node.clientFile(t, dep, "admin"), "--bundle", bundle, "--sums", sums)

	require.NoError(t, err, out)
	assert.Equal(t, []string{"GET /v1/config", "POST /v1/upgrade"}, node.requests, "the Node's extensions are checked first")
	sent, err := os.ReadFile(bundle)
	require.NoError(t, err)
	assert.Equal(t, sent, node.upgradeBody)
	h := sha256.Sum256(sent)
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
		renamed := filepath.Join(filepath.Dir(bundle), "naru-host-upgrade-v3.tar")
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

func TestUpgrade_ARefusalKeepsTheFieldsTheNodeNames(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	node.upgradeRefusal = map[string]any{"error": "the bundle is not an upgrade", "fields": []string{"sha256", "bundle"}}
	bundle, sums := bundleWithSums(t, "the bundle", "")

	out, err := run(t, "upgrade", "--config", node.clientFile(t, dep, "admin"), "--bundle", bundle, "--sums", sums)

	require.Error(t, err)
	assert.Equal(t, "422 Unprocessable Entity: the bundle is not an upgrade (fields: sha256, bundle)", out)
}

func TestUpgrade_RefusesBeforePostingWhenAnExtensionEntryHasNoStoredImageBuiltForTheBundlesVersion(t *testing.T) {
	old, next := strings.Repeat("a", 64), strings.Repeat("c", 64)
	entry := func(sha string) map[string]any { return map[string]any{"name": "naru-orch", "sha256": sha} }
	tests := map[string]struct {
		entries []map[string]any
		refused bool
	}{
		"an entry for the old version only":                     {[]map[string]any{entry(old)}, true},
		"an entry whose image is not stored":                    {[]map[string]any{entry(old), entry(next)}, true},
		"an entry for the old and one for the new, both stored": {[]map[string]any{entry(old), entry(strings.Repeat("d", 64))}, false},
		"no extension entry":                                    {nil, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dep := newDeployment(t)
			apiAuthority(t, dep)
			node := newFakeNode(t, dep)
			node.extensionEntries = tt.entries
			node.storedExtensions = []map[string]any{
				{"sha256": old, "name": "naru-orch", "version": "v1"},
				{"sha256": strings.Repeat("d", 64), "name": "naru-orch", "version": "v2"},
			}
			bundle, sums := bundleWithSums(t, "the bundle", "")

			out, err := run(t, "upgrade", "--config", node.clientFile(t, dep, "admin"), "--bundle", bundle, "--sums", sums)

			if !tt.refused {
				require.NoError(t, err, out)
				assert.Contains(t, node.requests, "POST /v1/upgrade")
				return
			}
			require.Error(t, err)
			assert.Contains(t, out, "naru-orch")
			assert.Contains(t, out, "ADR 0055")
			assert.NotContains(t, node.requests, "POST /v1/upgrade", "nothing is posted")
		})
	}
}
