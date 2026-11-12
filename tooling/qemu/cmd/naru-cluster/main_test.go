package main_test

import (
	"os"
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

// stubRelease is a release directory whose naructl answers `topology` with the services of the
// stand-in layout the test names, and which holds nothing else: the disks are never converted.
func stubRelease(t *testing.T, topology string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naru-guest-v1.json"), []byte("{}"), 0o600))
	script := "#!/bin/sh\n[ \"$1\" = topology ] && cat <<'EOF'\n" + topology + "\nEOF\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naructl-v1-linux-amd64"), []byte(script), 0o755)) //nolint:gosec // a test executable
	return dir
}

const gaonTopology = `{"services":[{"name":"registry","controlPlane":false,"gaon":false},
{"name":"etcd","controlPlane":true,"gaon":false},{"name":"gaon-apiserver","controlPlane":true,"gaon":true},
{"name":"gaon-scheduler","controlPlane":true,"gaon":true},{"name":"gaon-controllers","controlPlane":true,"gaon":true},
{"name":"gaon-node-agent","controlPlane":false,"gaon":true}],"signer":"gaon-controllers","apiserver":"gaon-apiserver","agent":"gaon-node-agent"}`

func TestNaruCluster_AnUnknownServiceToDisableIsAnErrorThatNamesTheValidOnes(t *testing.T) {
	out, err := naruCluster(t, "-release", stubRelease(t, gaonTopology), "-dir", t.TempDir(), "-disable", "gaon-controllers,gaon-nothing")

	require.Error(t, err)
	assert.Contains(t, out, `"gaon-nothing" is not a service`)
	assert.Contains(t, out, "gaon-controllers", "the message lists the names")
}

func TestNaruCluster_TheServicesToDisableAreTheReleasesNaructls(t *testing.T) {
	renamed := `{"services":[{"name":"alpha","controlPlane":false,"gaon":false},{"name":"sign","controlPlane":true,"gaon":true}],"signer":"sign","apiserver":"sign","agent":"alpha"}`

	out, err := naruCluster(t, "-release", stubRelease(t, renamed), "-dir", t.TempDir(), "-disable", "gaon-scheduler")
	require.Error(t, err)
	assert.Contains(t, out, `"gaon-scheduler" is not a service; the names are alpha, sign`)

	out, err = naruCluster(t, "-release", stubRelease(t, renamed), "-dir", t.TempDir(), "-disable", "sign")
	require.Error(t, err)
	assert.Contains(t, out, "-disable sign needs -boot-pki=false")
}

func TestNaruCluster_AReleaseWhoseNaructlHasNoTopologyIsRefusedAndTheErrorSaysSo(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naru-guest-v1.json"), []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naructl-v1-linux-amd64"), []byte("#!/bin/sh\necho usage >&2\nexit 1\n"), 0o755)) //nolint:gosec // a test executable

	out, err := naruCluster(t, "-release", dir, "-dir", t.TempDir())

	require.Error(t, err)
	assert.Contains(t, out, "has no topology command")
}

func TestNaruCluster_AKnownServiceToDisablePassesTheCheckAndFailsLaterOnTheStubRelease(t *testing.T) {
	// The control for the refusal above: the same arguments with a valid name get past the check.
	out, err := naruCluster(t, "-release", stubRelease(t, gaonTopology), "-dir", t.TempDir(), "-disable", "gaon-scheduler")

	require.Error(t, err)
	assert.NotContains(t, out, "is not a service")
	assert.Contains(t, out, "read the release")
}

func TestNaruCluster_DisablingTheControllersNeedsBootPKIFalseAndIsRefusedBeforeTheDisksAreRead(t *testing.T) {
	out, err := naruCluster(t, "-release", stubRelease(t, gaonTopology), "-dir", t.TempDir(), "-disable", "gaon-controllers")

	require.Error(t, err)
	assert.Contains(t, out, "-boot-pki=false")
	assert.NotContains(t, out, "read the release")

	out, err = naruCluster(t, "-release", stubRelease(t, gaonTopology), "-dir", t.TempDir(), "-disable", "gaon-controllers", "-boot-pki=false")
	require.Error(t, err)
	assert.Contains(t, out, "read the release", "with -boot-pki=false it gets past the check")
}

func TestNaruCluster_ADirectoryThatIsNotEmptyIsRefusedAndNamed(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cluster.json"), []byte("{}"), 0o600))

	out, err := naruCluster(t, "-release", t.TempDir(), "-dir", dir)

	require.Error(t, err)
	assert.Contains(t, out, "-dir "+dir+" is not empty")
	assert.NotContains(t, out, "read the release", "the check comes before anything is read or written")
}

func TestNaruCluster_TheReleaseAndTheDirectoryAreRequired(t *testing.T) {
	out, err := naruCluster(t)

	require.Error(t, err)
	assert.Contains(t, out, "-release and -dir are required")
}
