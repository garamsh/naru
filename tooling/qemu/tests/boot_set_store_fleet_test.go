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

	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/seed"
)

const (
	// storeDir is where the control Nodes keep the release store orch reads (ADR 0089).
	storeDir = "/var/lib/naru/boot-set-store"
	// storeVersion is a Node version no host offers a boot set for.
	storeVersion = "v9.9.9"
	// signerIdentity is the identity the store's releases are checked against.
	signerIdentity = "https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/*"
)

// storeFiles puts the files of dir into every Node's user-data as the store orch reads, and has
// orchd take it with the boot-set flags (spec.orch.bootSetStore, bootSetIdentity).
func storeFiles(dir string) fleetOpts {
	return fleetOpts{
		controls: 1, hosts: 1,
		userData: func(_ node.Config, file string) error {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				b, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return err
				}
				if err := seed.Append(file, map[string]any{"path": storeDir + "/" + e.Name(), "mode": "0644", "content": string(b)}, "spec", "files"); err != nil {
					return err
				}
			}
			return nil
		},
		orchExtra: func(int) map[string]any {
			return map[string]any{"bootSetStore": storeDir, "bootSetIdentity": signerIdentity}
		},
	}
}

// osGroupFile is the OS-form group whose boot set no host offers.
func (f *fleet) osGroupFile(name string) string {
	return f.file(name+".yaml", fmt.Sprintf(`name: %s
count: 1
region: eu-1
template:
  form: os
  cpus: 1
  memoryMiB: 256
  nodeVersion: %s
  disks:
    - {name: data, sizeBytes: 1073741824}
rollout: {maxUnavailable: 1, maxSurge: 1}
`, name, storeVersion))
}

// A store whose bundle does not verify leaves the group held with bootset.failed in the events, and
// no boot set reaches the host.
func TestFleet_AStoreWhoseSignatureDoesNotVerifyLeavesTheGroupHeldWithABootSetFailure(t *testing.T) {
	dir := t.TempDir()
	kernel, initramfs := []byte("not a kernel"), []byte("not an initramfs")
	for name, b := range map[string][]byte{
		"vmlinuz": kernel, "initramfs.img": initramfs,
		"naru-node-" + storeVersion + ".json": []byte(`{"name":"naru-node-v9-9-9","version":"` + storeVersion + `"}`),
		"SHA256SUMS":                          []byte(strings.Repeat("0", 64) + "  vmlinuz\n"),
		"SHA256SUMS.sigstore.json":            []byte(`{"mediaType":"not a bundle"}`),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o600))
	}
	f := newFleet(t, storeFiles(dir))
	f.startOrch()
	f.enrollAndAdmit("eu-1")

	f.must(f.orch("group", "apply", "-f", f.osGroupFile("bs")), "apply the OS-form group")

	eventually(t, "the host's failure to get the boot set is in the events", func() bool {
		r := f.orch("events")
		return r.Err == nil && strings.Contains(r.Stdout, "bootset.failed") && strings.Contains(r.Stdout, "signature check failed")
	})
	eventually(t, "the group is held for the node version", func() bool {
		r := f.orch("group", "get", "bs")
		return r.Err == nil && strings.Contains(r.Stdout, "held at ") && strings.Contains(r.Stdout, "node version")
	})
	g, ok := f.group(1, "bs")
	require.True(t, ok)
	assert.Zero(t, g.count("running"), "no member runs on a host that has no boot set")
	assert.NotContains(t, f.must(f.orch("events"), "events"), "bootset.added")
}

// A group of the OS form whose version no host has reaches Running with no boot-set add: orch
// verifies the signed release in the store and streams its boot set to the host. The store is the
// assets of a published release (a kernel and an initramfs of its size do not travel in user-data
// otherwise), so the test needs NARU_BOOT_SET_RELEASE_DIR and NARU_BOOT_SET_RELEASE_VERSION.
func TestFleet_AnOSGroupWhoseVersionNoHostHasReachesRunningWithNoManualBootSetAdd(t *testing.T) {
	dir, version := os.Getenv("NARU_BOOT_SET_RELEASE_DIR"), os.Getenv("NARU_BOOT_SET_RELEASE_VERSION")
	if dir == "" || version == "" {
		t.Skip("NARU_BOOT_SET_RELEASE_DIR and NARU_BOOT_SET_RELEASE_VERSION name a signed release's assets and its version")
	}
	f := newFleet(t, storeFiles(dir))
	f.startOrch()
	f.enrollAndAdmit("eu-1")

	file := f.file("bs-ok.yaml", strings.ReplaceAll(strings.TrimPrefix(fmt.Sprintf(`name: bs-ok
count: 1
region: eu-1
template:
  form: os
  cpus: 1
  memoryMiB: 256
  nodeVersion: %s
  disks:
    - {name: data, sizeBytes: 1073741824}
rollout: {maxUnavailable: 1, maxSurge: 1}
`, version), "\n"), "\t", "  "))
	f.must(f.orch("group", "apply", "-f", file), "apply the OS-form group")

	eventually(t, "the member runs", func() bool {
		g, ok := f.group(1, "bs-ok")
		return ok && g.count("running") == 1
	})
	assert.Contains(t, f.must(f.orch("events"), "events"), "bootset.added")
}
