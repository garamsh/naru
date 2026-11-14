package command_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtensionAdd_StreamsTheImageUnderItsOwnDigest(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	image := filepath.Join(t.TempDir(), "naru-orch-ext-v9.9.9.sqfs")
	require.NoError(t, os.WriteFile(image, []byte("squashfs bytes"), 0o600))
	sum := sha256.Sum256([]byte("squashfs bytes"))
	digest := hex.EncodeToString(sum[:])

	out, err := run(t, "extension", "add", image, "--config", node.clientFile(t, dep, "admin"), "--node", node.addr())

	require.NoError(t, err, out)
	assert.Equal(t, "/v1/extensions/"+digest, node.extensionPath)
	assert.Equal(t, "squashfs bytes", string(node.extensionBody))
	assert.Contains(t, out, "name: naru-orch, sha256: "+digest, "the line to put in spec.extensions")
}

func TestExtensionAdd_ARoleThatCannotChangeTheNodeIsRefused(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	image := filepath.Join(t.TempDir(), "x.sqfs")
	require.NoError(t, os.WriteFile(image, []byte("x"), 0o600))

	_, err := run(t, "extension", "add", image, "--config", node.clientFile(t, dep, "reader"), "--node", node.addr())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
}

func TestExtensionList_PrintsEachImageAndWhereItIsMounted(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)

	out, err := run(t, "extension", "list", "--config", node.clientFile(t, dep, "reader"), "--node", node.addr())

	require.NoError(t, err, out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"NAME", "VERSION", "SHA256", "SIZE", "SERVICES", "MOUNTED"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"naru-orch", "v9.9.9", strings.Repeat("a", 64), "1234", "naru-orchd,naru-signer", "/usr/lib/naru/extensions/naru-orch"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"naru-orch", "v9.9.8", strings.Repeat("b", 64), "99", "naru-orchd", "no"}, strings.Fields(lines[2]))
}

func TestExtensionRemove_RemovesByDigestAndReportsTheNodesRefusal(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "admin")

	out, err := run(t, "extension", "remove", strings.Repeat("b", 64), "--config", cfg, "--node", node.addr())
	require.NoError(t, err, out)
	assert.Equal(t, "/v1/extensions/"+strings.Repeat("b", 64), node.extensionRemoved)

	_, err = run(t, "extension", "remove", strings.Repeat("a", 64), "--config", cfg, "--node", node.addr())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "409")
	assert.Contains(t, err.Error(), "mounted")

	_, err = run(t, "extension", "remove", "nothex", "--config", cfg, "--node", node.addr())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a lowercase hex SHA-256")
}

func TestExtension_NeedsASubcommand(t *testing.T) {
	_, err := run(t, "extension")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add, list or remove")
	_, err = run(t, "extension", "drop")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not add, list or remove")
}
