//go:build qemu_real

package tests_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The test of the shutdown (ADR 0022) gives a Node a mounted system extension, reboots it, and reads
// what init logged as it undid the mounts once the services were stopped: the extension first, then
// the data disk, each unmounted and none refused.
func TestNode_AShutdownUnmountsTheExtensionThenTheDataDiskAndLogsNoFailure(t *testing.T) {
	c := nodeAPI(t)
	admin, ext := c.API.Admin, inputs.Extension
	nd := c.Nodes[0]
	patch := "spec:\n  extensions:\n    - {name: naru-orch, sha256: " + ext.SHA256 + "}\n"
	r := ctl(admin, "apply", "-f", patchFile(t, patch), "--mode", "staged")
	require.NoError(t, r.Err, r.refusal())
	require.NoError(t, nd.Reboot())
	eventually(t, "the Node API answers after the boot", func() bool { return ctl(admin, "version").Err == nil })
	require.Contains(t, extensionRows(t, admin), ext.SHA256)

	r = ctl(admin, "reboot")
	require.NoError(t, r.Err, r.refusal())
	select {
	case <-nd.Done():
	case <-time.After(bootTimeout):
		t.Fatal("the Node did not reboot")
	}

	console := nd.Console()
	const unmounted = `msg="unmounted at shutdown"`
	extension := strings.LastIndex(console, unmounted+` kind=extension mount=/usr/lib/naru/extensions/naru-orch`)
	dataDisk := strings.LastIndex(console, unmounted+` kind="data disk"`)
	require.GreaterOrEqual(t, extension, 0, "the extension was unmounted")
	require.GreaterOrEqual(t, dataDisk, 0, "the data disk was unmounted")
	assert.Less(t, extension, dataDisk, "the extension is unmounted before the data disk")
	assert.NotContains(t, console[extension:], `msg="unmount at shutdown failed"`, "no unmount was refused")
}
