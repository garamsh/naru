//go:build qemu_real

package tests_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/tooling/qemu/internal/guest"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// An upgrade whose image has another guest manifest than the one the host runs holds with no step
// of the operator's: mvmd's configuration is rendered from the guest.json of the image that boots, so
// the digests it checks are the new image's (ADR 0106). The `guest` upgrade image is the next version's
// tree on a guest initrd with bytes after its trailer, which `make image-upgrade` builds, so its
// guest.json names another digest than the image's own. The control keeps the old digests in the stored
// configuration, as a configuration stored by an earlier naructl does, and the same upgrade rolls back.

// changedGuest is the guest manifest of the `guest` upgrade image, and fails the test when it is the
// image's own: the cases below would then prove nothing.
func changedGuest(t *testing.T) guest.Manifest {
	t.Helper()
	g, err := guest.Read(filepath.Join(os.Getenv("NARU_UPGRADE_DIR"), "guest", "guest.json"))
	require.NoError(t, err)
	require.NotEqual(t, inputs.Guest.GuestInitrd.SHA256, g.GuestInitrd.SHA256, "the upgrade image's guest initrd has another digest than the image's own")
	return g
}

// guestVersion is the version of the `guest` upgrade image.
func guestVersion(t *testing.T) string {
	t.Helper()
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	return want + "-guest"
}

func TestNode_UpgradeAcrossAChangedGuestManifestHoldsWithTheConfigurationAsItWas(t *testing.T) {
	changed := guestVersion(t)
	changedGuest(t)
	nd, admin := startUpgradeNode(t)
	bundle, sums := bundleFor(t, "guest", changed)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	awaitRestarting(t, nd, "the Node runs the image with the changed guest manifest and has promoted it", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && s.Version == changed && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	assert.NotContains(t, nd.Console(), "initrd has digest", "mvmd took the digests of the image that booted")
}

func TestNode_UpgradeAcrossAChangedGuestManifestRollsBackWhenTheStoredConfigurationPinsTheOldDigests(t *testing.T) {
	changed := guestVersion(t)
	changedGuest(t)
	nd, admin := startUpgradeNode(t)
	pinOldDigests(t, nd, admin)
	bundle, sums := bundleFor(t, "guest", changed)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	awaitRestarting(t, nd, "the Node says the upgrade rolled back", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && strings.Contains(s.Outcome, "rolled back")
	})
	after, err := statusOf(tool, admin)
	require.NoError(t, err)
	assert.Equal(t, os.Getenv("NARU_VERSION"), after.Version, "the Node is on the version before")
	assert.Contains(t, nd.Console(), "initrd has digest", "mvmd refused the new image's guest initrd against the pinned digest")
}

// pinOldDigests replaces mvmd's stored configuration with the one the image the Node runs renders, so
// it holds that image's digests as a configuration stored by an earlier naructl does.
func pinOldDigests(t *testing.T, nd *node.Node, admin string) {
	t.Helper()
	var stored struct {
		Spec struct {
			Files []config.File `yaml:"files"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(storeOf(t, nd), &stored))
	var tmpl *config.File
	for i, f := range stored.Spec.Files {
		if f.Path == "/run/mvmd/config.json" {
			tmpl = &stored.Spec.Files[i]
		}
	}
	require.NotNil(t, tmpl, "the stored configuration has mvmd's file")
	require.Equal(t, config.TemplateGuest, tmpl.Template, "the stored file is a template of the guest manifest")
	pinned, err := tmpl.Render(func() ([]byte, error) { return json.Marshal(inputs.Guest) })
	require.NoError(t, err)
	patch, err := yaml.Marshal(map[string]any{"spec": map[string]any{"files": []map[string]any{
		{"path": tmpl.Path, "mode": tmpl.Mode, "template": "", "content": string(pinned)},
	}}})
	require.NoError(t, err)
	r := ctl(admin, "apply", "-f", patchFile(t, string(patch)))
	require.NoError(t, r.Err, r.refusal())
}

// A host that joined by enrollment has the configuration orch built from the fleet profile, whose mvmd
// file the Node API shows redacted, and the profile holds no guest manifest: the upgrade needs no
// hand-made configuration, and the host reaches Ready on the new image's digests.
func TestFleet_AJoinedHostUpgradesAcrossAChangedGuestManifestWithNoHandMadeConfiguration(t *testing.T) {
	changed := guestVersion(t)
	changedGuest(t)
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1, upgradable: true, forward: []string{"50000"}})
	f.startOrch()
	h := f.hosts()[0]
	id := f.enrollAndAdmit("eu-1")[h]
	api := f.forward(h, "50000")
	operator := f.operator("ops-1", "operator")
	bundle, sums := bundleFor(t, "guest", changed)
	call := func(words ...string) result {
		args := append(append([]string{}, words...), "--identity", operator.file, "--passphrase-file", f.pass, "--host-ca", f.c.HostCA, "--endpoint", api, "--server-name", id)
		return nrun(args...)
	}

	f.must(call("upgrade", "--bundle", bundle, "--sums", sums), "an operator upgrades the joined host to the image with the changed guest manifest")

	awaitRestarting(t, f.node(h), "the joined host runs the changed image and has promoted it", func() bool {
		r := call("upgrade", "--status")
		if r.Err != nil {
			return false
		}
		s, err := parseUpgradeState(r.Stdout)
		return err == nil && s.Version == changed && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	assert.NotContains(t, f.node(h).Console(), "initrd has digest", "mvmd took the digests of the image that booted")
}
