//go:build qemu_real

package tests_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The test of A/B boot (ADR 0020) runs on the boot disk the build made, with its ESP writable: the
// running system creates the trial boot entry, which the disk cannot hold because firmware keeps
// entries in NVRAM, sets BootNext, and the Node is power-cycled twice. The Node's NVRAM file
// persists across the cycles, as a real machine's does.

const bootNextYAML = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: naru-node-1
spec:
  dataDisk:
    label: ` + node.DataLabel + `
    mountPoint: ` + node.DataMount + `
  services:
    - name: naru56-bootnext
      binary: ` + node.DataMount + `/inputs/bin/naru-bootnext
`

func copyBootDisk(t *testing.T) string {
	t.Helper()
	in, err := os.Open(filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"))
	require.NoError(t, err)
	defer func() { _ = in.Close() }()
	p := filepath.Join(t.TempDir(), "naru.img")
	out, err := os.Create(p)
	require.NoError(t, err)
	_, err = io.Copy(out, in)
	require.NoError(t, err)
	require.NoError(t, out.Close())
	return p
}

func TestNode_BootNextBootsTheTrialSlotOnceAndTheNextRebootTheDefault(t *testing.T) {
	cfg := node.Config{Name: "node-1", Index: 1, Total: 1, Inputs: inputs}
	n, err := node.Start(node.Spec{
		Config: cfg, Disk: copyBootDisk(t), WritableDisk: true, Dir: t.TempDir(), Mcast: "230.78.0.1:25100", NaruYAML: []byte(bootNextYAML),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		n.Stop()
		t.Logf("console of %s:\n%s", n.Name, n.Console())
	})
	ctx := context.Background()

	// Boot 1: the default slot. The running system armed the trial slot and BootNext; the disk
	// held no entry for it.
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "naru56-efi: armed Boot0100 and BootNext=0100"))
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "naru56-efi: BootCurrent=", "BootNext=0100", "trial=true"))
	assert.NotContains(t, n.Console(), "BootCurrent=0100", "boot 1 is not the trial entry")
	first := bootCurrent(t, n.Console())
	logEFI(t, "boot 1 (default slot)", n.Console())

	// Boot 2: the firmware boots the trial entry, and deletes BootNext when it does.
	require.NoError(t, n.Reboot())
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "naru56-efi: BootCurrent=0100", "BootNext=none", "trial=true"))
	assert.NotContains(t, n.Console(), "armed", "boot 2 did not arm again: the entry and BootNext were already there")
	logEFI(t, "boot 2 (after BootNext)", n.Console())

	// Boot 3: BootNext is gone, so the default slot boots again.
	require.NoError(t, n.Reboot())
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "naru56-efi: BootCurrent=", "BootNext=none", "trial=true"))
	assert.NotContains(t, n.Console(), "BootCurrent=0100", "boot 3 is not the trial entry: BootNext was used once")
	assert.Equal(t, first, bootCurrent(t, n.Console()), "boot 3 is the entry boot 1 was")
	logEFI(t, "boot 3 (next reboot)", n.Console())
}

// logEFI logs the lines of the boot that the service wrote, so the run shows what each boot was.
func logEFI(t *testing.T, boot, console string) {
	t.Helper()
	for _, line := range strings.Split(console, "\n") {
		if strings.Contains(line, "naru56-efi:") || strings.Contains(line, "BdsDxe: starting") {
			t.Logf("%s: %s", boot, strings.TrimSpace(line))
		}
	}
}

var currentLine = regexp.MustCompile(`naru56-efi: BootCurrent=([0-9A-F]{4})`)

func bootCurrent(t *testing.T, console string) string {
	t.Helper()
	m := currentLine.FindStringSubmatch(console)
	require.NotNil(t, m, console)
	return m[1]
}
