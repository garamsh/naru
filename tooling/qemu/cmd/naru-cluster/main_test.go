package main_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// naruCluster builds the command once per test and runs it with args, returning its combined output.
func naruCluster(t *testing.T, args ...string) (string, error) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "naru-cluster")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, string(out))
	out, err = exec.Command(bin, args...).CombinedOutput() //nolint:gosec // the binary just built
	return string(out), err
}

func TestNaruCluster_AnUnknownServiceToDisableIsAnErrorThatNamesTheValidOnes(t *testing.T) {
	out, err := naruCluster(t, "-release", t.TempDir(), "-dir", t.TempDir(), "-disable", "gaon-controllers,gaon-nothing")

	require.Error(t, err)
	assert.Contains(t, out, `"gaon-nothing" is not a service`)
	assert.Contains(t, out, "gaon-controllers", "the message lists the names")
}

func TestNaruCluster_AKnownServiceToDisablePassesTheCheckAndFailsLaterOnTheEmptyRelease(t *testing.T) {
	// The control for the refusal above: the same arguments with a valid name get past the check.
	out, err := naruCluster(t, "-release", t.TempDir(), "-dir", t.TempDir(), "-disable", "gaon-controllers")

	require.Error(t, err)
	assert.NotContains(t, out, "is not a service")
	assert.Contains(t, out, "read the release")
}

func TestNaruCluster_TheReleaseAndTheDirectoryAreRequired(t *testing.T) {
	out, err := naruCluster(t)

	require.Error(t, err)
	assert.Contains(t, out, "-release and -dir are required")
}
