//go:build qemu_real

package tests_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// bundleOf writes the upgrade bundle of the kernel at kernelPath as the release would, with the ESP
// of the image the Node boots recorded in its manifest, and its SHA256SUMS, and returns their paths.
func bundleOf(t *testing.T, kernelPath, version string) (bundle, sums string) {
	t.Helper()
	esp, err := release.ESPBytes(filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"))
	require.NoError(t, err)
	dir := t.TempDir()
	bundle = filepath.Join(dir, release.BundleName(version))
	require.NoError(t, release.WriteBundle(bundle, version, kernelPath, esp))
	b, err := os.ReadFile(bundle)
	require.NoError(t, err)
	sums = filepath.Join(dir, "SHA256SUMS")
	require.NoError(t, os.WriteFile(sums, []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(b), filepath.Base(bundle))), 0o600))
	return bundle, sums
}

// paddedKernel is the `next` kernel of make image-upgrade followed by zeros up to bytes: a kernel
// of the size a later release may have, which the firmware loads as the kernel it is (the PE
// header gives the image's size, and what follows it is not read).
func paddedKernel(t *testing.T, bytes int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bzImage")
	in, err := os.ReadFile(filepath.Join(os.Getenv("NARU_UPGRADE_DIR"), "next", "bzImage"))
	require.NoError(t, err)
	require.Less(t, int64(len(in)), bytes, "the padding adds to the kernel")
	require.NoError(t, os.WriteFile(p, in, 0o600))
	require.NoError(t, os.Truncate(p, bytes))
	return p
}

func TestNode_ANodeOfTheCurrentImageTakesAnUpgradeToAKernelTwiceTheSizeOfItsOwn(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	next := want + "-next"
	installed, err := os.Stat(filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "bzImage"))
	require.NoError(t, err)
	esp, err := release.ESPBytes(filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"))
	require.NoError(t, err)
	grown := 2 * installed.Size()
	t.Logf("installed kernel %d bytes, ESP %d bytes, the upgrade's kernel %d bytes, trial room %d bytes", installed.Size(), esp, grown, esp-installed.Size())
	nd, admin := startUpgradeNode(t)
	before, err := statusOf(admin)
	require.NoError(t, err)
	require.Equal(t, want, before.Version)
	bundle, sums := bundleOf(t, paddedKernel(t, grown), next)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	assert.Contains(t, r.Stdout, "the Node accepted the upgrade")
	awaitRestarting(t, nd, "the Node runs the larger kernel and has promoted it", func() bool {
		s, err := statusOf(admin)
		return err == nil && s.Version == next && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	after, err := statusOf(admin)
	require.NoError(t, err)
	assert.NotEqual(t, before.BootID, after.BootID, "an upgrade is a new boot")
	assert.Contains(t, after.Outcome, want+" -> "+next+", promoted")
}
