//go:build qemu_real

package tests_test

import (
	"fmt"
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
	c := plainRelease
	c.Configure = func(c *node.Config, g *naructl.Node) {
		withExtraDisk(c)
		for _, s := range c.Storage {
			g.Storage = append(g.Storage, s.Name+"="+s.Device)
		}
	}
	c.Installed = func(t *testing.T, r releaseRun) {
		require.True(t, r.Node.Has(formatted, "device=/dev/vdb"), "the control: the first boot formatted the storage disk the entry names")
		require.True(t, r.Node.Has(storageMark, "device=/dev/vdb"), "and mounted it")
	}
	c.Upgraded = func(t *testing.T, r releaseRun, mark int) {
		assert.True(t, r.Node.HasAfter(mark, storageMark, "device=/dev/vdb"), "the upgraded boot mounted the storage disk again")
		assert.False(t, r.Node.HasAfter(mark, "storage disk not prepared"), "and did not refuse its entry")
		assert.False(t, r.Node.HasAfter(mark, "boot refused"), "and refused no part of the boot")
	}
	upgradeBetweenReleases(t, c)
}

// A control host whose spec.extensions names naru-orch without its digest takes the new release's
// image with the upgrade, as ADR 0084 decides and #265 asks: the operator posts the bundle and does
// nothing else. A host installed from a release before ADR 0084 cannot name it that way, so the test
// skips for one, and make test-release-upgrade allows that skip.
func TestRelease_AControlHostRunningTheExtensionUpgradesWithTheBundleAloneAndMountsTheNewImage(t *testing.T) {
	var oldImage, newImage string
	c := plainRelease
	c.Skip = func(from string) string {
		if fixture.PredatesCarriedExtensions(from) {
			return fmt.Sprintf("%s predates %s, the first release whose upgrade carries a host's extensions (ADR 0084); its host cannot name naru-orch without a digest", from, fixture.FirstCarriedExtensions)
		}
		return ""
	}
	c.Configure = func(_ *node.Config, g *naructl.Node) { g.Extra = append(g.Extra, "--extension", fixture.ExtensionName) }
	c.Installed = func(t *testing.T, r releaseRun) {
		info, err := fixture.ReadExtensionInfo(filepath.Join(r.ToDir, fixture.ReleaseExtensionName(r.To, ".json")))
		require.NoError(t, err)
		oldImage, newImage = r.From.Inputs.Extension.SHA256, info.SHA256
		require.NotEqual(t, oldImage, newImage, "the two releases carry two images of naru-orch")
		eventually(t, "the control: the Node mounts the extension of the release it was installed from", func() bool {
			rows, err := extensionTableWith(r.FromTool, r.Admin)
			return err == nil && mountedExtension(rows[oldImage])
		})
	}
	c.Upgraded = func(t *testing.T, r releaseRun, _ int) {
		eventually(t, "the Node mounts the image the bundle carried and no longer stores the old one", func() bool {
			rows, err := extensionTableWith(r.ToTool, r.Admin)
			_, oldStored := rows[oldImage]
			return err == nil && mountedExtension(rows[newImage]) && !oldStored
		})
	}
	upgradeBetweenReleases(t, c)
}

// releaseCase is what a release upgrade test adds to the host it installs. Skip names the reason
// the release in FROM cannot have the host, or is empty. Configure sets the host on the Node's
// configuration and on the host `gen node` writes. Installed checks the boot of the release in FROM,
// and Upgraded the host after the upgrade; mark is the length of the console when the upgrade began.
type releaseCase struct {
	Skip      func(from string) string
	Configure func(*node.Config, *naructl.Node)
	Installed func(t *testing.T, r releaseRun)
	Upgraded  func(t *testing.T, r releaseRun, mark int)
}

// releaseRun is the Node a release case installed and what its checks reach it with.
type releaseRun struct {
	Node             *node.Node
	Admin            string
	FromTool, ToTool naructl.Tool
	From             fixture.Release
	ToDir, To        string
}

// plainRelease is the case of a host with nothing added.
var plainRelease = releaseCase{
	Skip:      func(string) string { return "" },
	Configure: addNothing,
	Installed: func(*testing.T, releaseRun) {},
	Upgraded:  func(*testing.T, releaseRun, int) {},
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
	if reason := c.Skip(from.Version); reason != "" {
		t.Skip(reason)
	}
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
	run := releaseRun{Node: nd, Admin: admin, FromTool: fromTool, ToTool: toTool, From: from, ToDir: toDir, To: to}
	c.Installed(t, run)
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
	c.Upgraded(t, run, mark)
}

// mountedExtension reports whether a row of `naructl extension list` is mounted, which is the
// path its last column shows.
func mountedExtension(row []string) bool {
	return len(row) > 0 && row[len(row)-1] == "/usr/lib/naru/extensions/"+fixture.ExtensionName
}
