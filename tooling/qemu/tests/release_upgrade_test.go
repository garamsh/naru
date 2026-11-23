//go:build qemu_real

package tests_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The release upgrade test installs a published release and upgrades it in place to the next one.
// It reads two release directories as `gh release download <tag>` writes them, and takes both
// versions from their files, never from the tree's: the tree is not what an operator ran.

// fromReleaseEnv and toReleaseEnv name the directories of the release to install and the one to
// upgrade to; make test-release-upgrade sets them.
const (
	fromReleaseEnv = "NARU_FROM_RELEASE_DIR"
	toReleaseEnv   = "NARU_TO_RELEASE_DIR"
)

func TestRelease_AHostInstalledFromOneReleaseUpgradesInPlaceToTheNextAndKeepsItsData(t *testing.T) {
	upgradeBetweenReleases(t, plainRelease)
}

// A gen node host with a storage disk and a static network-config upgraded in place was the case of
// a trial boot that was refused and rolled back (#263); no release test had it.
func TestRelease_AHostWithAStorageDiskUpgradesInPlaceToTheNextAndMountsItAgain(t *testing.T) {
	upgradeBetweenReleases(t, releaseCase{
		Configure: func(c *node.Config, g *naructl.Node) {
			withExtraDisk(c)
			for _, s := range c.Storage {
				g.Storage = append(g.Storage, s.Name+"="+s.Device)
			}
		},
		Installed: func(t *testing.T, nd *node.Node) {
			require.True(t, nd.Has(formatted, "device=/dev/vdb"), "the control: the first boot formatted the storage disk the entry names")
			require.True(t, nd.Has(storageMark, "device=/dev/vdb"), "and mounted it")
		},
		Upgraded: func(t *testing.T, nd *node.Node, mark int) {
			assert.True(t, nd.HasAfter(mark, storageMark, "device=/dev/vdb"), "the upgraded boot mounted the storage disk again")
			assert.False(t, nd.HasAfter(mark, "storage disk not prepared"), "and did not refuse its entry")
			assert.False(t, nd.HasAfter(mark, "boot refused"), "and refused no part of the boot")
		},
	})
}

// releaseCase is what a release upgrade test adds to the host it installs: Configure sets it on the
// Node's configuration and the host `gen node` writes, Installed checks the boot of the release in
// FROM, and Upgraded checks the console after mark, the length of the console when the upgrade began.
type releaseCase struct {
	Configure func(*node.Config, *naructl.Node)
	Installed func(t *testing.T, nd *node.Node)
	Upgraded  func(t *testing.T, nd *node.Node, mark int)
}

// plainRelease is the case of a host with nothing added.
var plainRelease = releaseCase{
	Configure: addNothing,
	Installed: func(*testing.T, *node.Node) {},
	Upgraded:  func(*testing.T, *node.Node, int) {},
}

// upgradeBetweenReleases installs the release in FROM on the host the case describes, upgrades it to
// the one in TO and checks the Node promoted it and the case's own checks.
func upgradeBetweenReleases(t *testing.T, c releaseCase) {
	t.Helper()
	fromDir, toDir := os.Getenv(fromReleaseEnv), os.Getenv(toReleaseEnv)
	var unset []string
	if fromDir == "" {
		unset = append(unset, fromReleaseEnv)
	}
	if toDir == "" {
		unset = append(unset, toReleaseEnv)
	}
	if len(unset) > 0 {
		t.Skipf("%s not set; make test-release-upgrade FROM=<dir> TO=<dir> sets both", strings.Join(unset, " and "))
	}
	from, err := fixture.ReadRelease(fromDir, t.TempDir())
	require.NoError(t, err)
	// An operator who installed a release has that release's naructl, and the one of the release
	// they upgrade to; each writes and reads what its release does, and one from before cobra is
	// driven with the flag spellings it was released with.
	fromTool := naructl.ForRelease(from.Naructl, from.Version)
	toBin, err := fixture.StageNaructl(toDir, t.TempDir())
	require.NoError(t, err)
	toTool := naructl.Tool{Bin: toBin}
	bundles, err := filepath.Glob(filepath.Join(toDir, "naru-host-upgrade-*.tar"))
	require.NoError(t, err)
	require.Len(t, bundles, 1, "%s holds one naru-host-upgrade-<tag>.tar", toDir)
	to := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(bundles[0]), "naru-host-upgrade-"), ".tar")
	require.NotEqual(t, from.Version, to, "the two directories hold two releases")

	nd, admin := startUpgradeNodeOn(t, fromTool, from.Inputs, from.BootDisk, c.Configure)
	before, err := statusOf(fromTool, admin)
	require.NoError(t, err)
	require.Equal(t, from.Version, before.Version, "the Node runs the release it was installed from")
	require.Equal(t, "default", before.Slot)
	require.NoError(t, ctlWith(fromTool, admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	stored := storeOf(t, nd)
	c.Installed(t, nd)
	mark := len(nd.Console())

	r := ctlWith(toTool, admin, "upgrade", "--bundle", bundles[0], "--sums", filepath.Join(toDir, "SHA256SUMS"))

	require.NoError(t, r.Err, r.refusal())
	awaitRestarting(t, nd, "the Node runs the release it was upgraded to and has promoted it", func() bool {
		s, err := statusOf(toTool, admin)
		return err == nil && s.Version == to && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	after, err := statusOf(toTool, admin)
	require.NoError(t, err)
	assert.Equal(t, to, after.Version, "the version is the release's")
	assert.NotEqual(t, before.BootID, after.BootID, "an upgrade is a new boot")
	assert.Contains(t, after.Outcome, from.Version+" -> "+to+", promoted")
	assert.Equal(t, stored, storeOf(t, nd), "the file on the data disk reads back the same")
	services := ctlWith(toTool, admin, "services")
	require.NoError(t, services.Err, services.refusal())
	assert.Contains(t, services.Stdout, "naru49-idle", "the Node lists the service the applied configuration names")
	c.Upgraded(t, nd, mark)
}
