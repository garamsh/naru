//go:build qemu_real

// Package harness_test drives naru-cluster as an external harness does: it runs the command from a
// directory outside the checkout against the output of `make release`, gives it the gaon binaries
// of the pin as -gaon-bin, and then uses only what cluster.json says: the API server's address,
// the CA and the client pair, the Nodes' pids and roles, and the image reference. `make
// test-harness` runs it.
package harness_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/clusterfile"
	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

const upTimeout = 6 * time.Minute

var (
	// naruCluster is the command, built once, and releaseDir and gaonDir are the inputs `make
	// test-harness` names.
	naruCluster, releaseDir, gaonDir string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	releaseDir, gaonDir = os.Getenv("NARU_RELEASE_DIR"), os.Getenv("NARU_GAON_DIR")
	if releaseDir == "" || gaonDir == "" {
		os.Stderr.WriteString("NARU_RELEASE_DIR and NARU_GAON_DIR are required; make test-harness sets them\n")
		return 1
	}
	bin, err := os.MkdirTemp("", "naru38-bin-")
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	defer func() { _ = os.RemoveAll(bin) }()
	naruCluster = filepath.Join(bin, "naru-cluster")
	if out, err := exec.Command("go", "build", "-o", naruCluster, "github.com/garamsh/naru/tooling/qemu/cmd/naru-cluster").CombinedOutput(); err != nil {
		os.Stderr.WriteString("build naru-cluster: " + err.Error() + "\n" + string(out))
		return 1
	}
	return m.Run()
}

// running is a naru-cluster process.
type running struct {
	cmd  *exec.Cmd
	dir  string
	log  string
	file clusterfile.File
}

// start runs naru-cluster with args from a directory outside the checkout, waits for cluster.json,
// and stops it when the test ends, which must leave no Node's process behind.
func start(t *testing.T, args ...string) *running {
	t.Helper()
	r := &running{dir: t.TempDir(), log: filepath.Join(t.TempDir(), "naru-cluster.log")}
	logFile, err := os.Create(r.log)
	require.NoError(t, err)
	absRelease, err := filepath.Abs(releaseDir)
	require.NoError(t, err)
	r.cmd = exec.Command(naruCluster, append([]string{"-release", absRelease, "-dir", r.dir}, args...)...) //nolint:gosec // the command just built
	r.cmd.Dir = t.TempDir()                                                                                // not the checkout
	r.cmd.Stdout, r.cmd.Stderr = logFile, logFile
	require.NoError(t, r.cmd.Start())
	exited := make(chan struct{})
	go func() { _ = r.cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		pids := []int{}
		for _, n := range r.file.Nodes {
			pids = append(pids, n.PID)
		}
		_ = r.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(time.Minute):
			_ = r.cmd.Process.Kill()
			t.Errorf("naru-cluster did not stop within a minute of SIGTERM")
		}
		for _, pid := range pids {
			if alive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				t.Errorf("the QEMU process %d outlived naru-cluster", pid)
			}
		}
		b, _ := os.ReadFile(r.log)
		t.Logf("naru-cluster log:\n%s", b)
	})

	path := filepath.Join(r.dir, clusterfile.Name)
	require.Eventually(t, func() bool {
		select {
		case <-exited:
			t.Fatalf("naru-cluster exited before cluster.json appeared")
		default:
		}
		_, err := os.Stat(path)
		return err == nil
	}, upTimeout, time.Second, "cluster.json appears once every Node is up")
	r.file, err = clusterfile.Read(path)
	require.NoError(t, err)
	return r
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// client is what a harness builds from cluster.json: the resource API's client with the pair the
// file names, and the gaon CLI's argument list for the same server.
func client(t *testing.T, f clusterfile.File) (*hostapi.Client, []string) {
	t.Helper()
	ca, err := os.ReadFile(f.PKI.CA)
	require.NoError(t, err)
	cert, err := os.ReadFile(f.PKI.ClientCert)
	require.NoError(t, err)
	key, err := os.ReadFile(f.PKI.ClientKey)
	require.NoError(t, err)
	api, err := hostapi.New(f.APIServer.Address, &pki.Cluster{CA: ca, Client: pki.Pair{CertPEM: cert, KeyPEM: key}})
	require.NoError(t, err)
	return api, []string{"--server", "https://" + f.APIServer.Address + hostapi.Root, "--tls-ca", f.PKI.CA, "--tls-cert", f.PKI.ClientCert, "--tls-key", f.PKI.ClientKey}
}

func gaonCLI(t *testing.T, global []string, args ...string) string {
	t.Helper()
	out, err := exec.Command(filepath.Join(gaonDir, "gaon"), append(append([]string{}, global...), args...)...).CombinedOutput() //nolint:gosec // the pin's CLI
	require.NoError(t, err, string(out))
	return string(out)
}

func admit(t *testing.T, global []string, name string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "node.yaml")
	require.NoError(t, os.WriteFile(p, []byte("apiVersion: compute.gaon.garam.sh/v1alpha1\nkind: Node\nmetadata:\n  name: "+name+"\nspec:\n  admitted: true\n"), 0o600))
	t.Logf("gaon apply Node %s: %s", name, strings.TrimSpace(gaonCLI(t, global, "apply", "-f", p)))
}

// markedBinaries copies the pin's four binaries into a new directory, each with one byte appended:
// the ELF still runs, and its digest differs from the image's, so the data disk shows which one a
// Node was given. It returns the directory and the digest of each.
func markedBinaries(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir, sums := t.TempDir(), map[string]string{}
	for _, name := range []string{"gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"} {
		b, err := os.ReadFile(filepath.Join(gaonDir, name))
		require.NoError(t, err, "make image builds the pin's binaries")
		b = append(b, 0)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), b, 0o755)) //nolint:gosec // an executable
		sum := sha256.Sum256(b)
		sums[name] = hex.EncodeToString(sum[:])
	}
	return dir, sums
}

var imageRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// TestCluster_AHarnessReachesTheClusterThroughClusterJSONAndSurvivesOneNodeBeingKilled boots two
// Nodes with the gaon binaries supplied and node-1 issuing its certificates at boot, applies a Node
// through the host address with the gaon CLI, reads the roles and the image reference, power-offs
// node-2 with SIGKILL, and shows node-1 still served and naru-cluster still running.
func TestCluster_AHarnessReachesTheClusterThroughClusterJSONAndSurvivesOneNodeBeingKilled(t *testing.T) {
	bin, sums := markedBinaries(t)
	r := start(t, "-n", "2", "-gaon-bin", bin)
	f := r.file

	assert.Equal(t, clusterfile.Version, f.Version)
	require.Len(t, f.Nodes, 2)
	assert.Equal(t, []bool{true, false}, []bool{f.Nodes[0].ControlPlane, f.Nodes[1].ControlPlane}, "node-1 is the control plane")
	require.NotEmpty(t, f.Images)
	assert.Regexp(t, imageRef, f.Images[0].Reference, "an image the Nodes' mvmd pull by digest")
	for _, n := range f.Nodes {
		assert.True(t, alive(n.PID), "%s runs as %d", n.Name, n.PID)
		assert.FileExists(t, n.SerialLog)
		b, err := os.ReadFile(n.SerialLog)
		require.NoError(t, err)
		assert.Contains(t, string(b), "service=gaon-node-agent", n.Name)
	}

	// The binaries supplied are the ones on every Node's data disk, where the services'
	// configuration names them (each Node's disk is its own file under -dir).
	for _, n := range f.Nodes {
		disk := node.OpenData(filepath.Join(r.dir, n.Name, "data.img"))
		for name, want := range sums {
			dump := filepath.Join(t.TempDir(), name)
			require.NoError(t, disk.Extract(node.OverrideDir+"/"+name, dump), "%s: %s", n.Name, name)
			b, err := os.ReadFile(dump)
			require.NoError(t, err)
			sum := sha256.Sum256(b)
			assert.Equal(t, want, hex.EncodeToString(sum[:]), "%s carries the supplied %s", n.Name, name)
		}
	}
	api, global := client(t, f)
	ctx := context.Background()
	for _, n := range f.Nodes {
		admit(t, global, n.Name)
	}
	for _, n := range f.Nodes {
		require.Eventually(t, func() bool {
			got, err := api.Node(ctx, n.Name)
			return err == nil && got.Status.State == "Admitted"
		}, upTimeout, time.Second, n.Name+" is Admitted")
	}

	// A kill of node-2 is a power-off: logged, not fatal, and node-1 keeps serving.
	require.NoError(t, syscall.Kill(f.Nodes[1].PID, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !alive(f.Nodes[1].PID) }, time.Minute, 100*time.Millisecond, "node-2's process is gone")
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(r.log)
		return strings.Contains(string(b), "node lost") && strings.Contains(string(b), "node-2")
	}, time.Minute, 100*time.Millisecond, "naru-cluster logs the loss")
	assert.True(t, alive(r.cmd.Process.Pid), "naru-cluster keeps running")
	assert.True(t, alive(f.Nodes[0].PID), "node-1 keeps running")
	got, err := api.Node(ctx, "node-1")
	require.NoError(t, err, "node-1's API server still answers")
	assert.True(t, got.Ready())
	assert.Contains(t, gaonCLI(t, global, "get", "node", "node-1"), "node-1", "and the CLI is still served")
}

// TestCluster_DisablingAServiceLeavesItOutOfEveryNodeAndTheOperatorFilesPathStillGivesAWorkingClientPair
// boots the cluster with -disable gaon-controllers and the certificates carried as files, with the
// pin's own gaon-bin, and shows that the controllers never start, that the rest of the control plane
// does, and that cluster.json's client pair is accepted: the Node applied through it is admitted.
func TestCluster_DisablingAServiceLeavesItOutOfEveryNodeAndTheOperatorFilesPathStillGivesAWorkingClientPair(t *testing.T) {
	r := start(t, "-n", "2", "-gaon-bin", gaonDir, "-boot-pki=false", "-disable", "gaon-controllers")
	f := r.file

	for _, n := range f.Nodes {
		b, err := os.ReadFile(n.SerialLog)
		require.NoError(t, err)
		console := string(b)
		assert.NotContains(t, console, "service=gaon-controllers", "%s never starts the controllers", n.Name)
		assert.Contains(t, console, "service=gaon-node-agent", n.Name)
		if n.ControlPlane {
			assert.Contains(t, console, "service=gaon-scheduler", "the rest of the control plane runs")
			assert.Contains(t, console, "service=gaon-apiserver")
		}
	}
	api, global := client(t, f)
	admit(t, global, "node-1")
	require.Eventually(t, func() bool {
		got, err := api.Node(context.Background(), "node-1")
		return err == nil && got.Status.State == "Admitted"
	}, upTimeout, time.Second, "node-1 is Admitted through the client pair of cluster.json")
}

// stub stands in for a gaon service: it says who it is and exits.
const stub = `package main

import (
	"fmt"
	"os"
)

func main() { fmt.Println("naru38-stub: I am", os.Args[0]); os.Exit(3) }
`

// TestCluster_ASuppliedBinaryIsWhatTheServiceRuns supplies a stub as gaon-scheduler and nothing
// else, and shows on node-1's console that init ran the stub from the supplied path for that
// service, while the services with no binary supplied run the image's.
func TestCluster_ASuppliedBinaryIsWhatTheServiceRuns(t *testing.T) {
	src, bin := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "main.go"), []byte(stub), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "go.mod"), []byte("module stub\n\ngo 1.24\n"), 0o600))
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "gaon-scheduler"), ".")
	build.Dir, build.Env = src, append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOWORK=off")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	r := start(t, "-n", "1", "-gaon-bin", bin)

	serial := r.file.Nodes[0].SerialLog
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(serial)
		return strings.Contains(string(b), "naru38-stub: I am /var/lib/naru/inputs/gaon/gaon-scheduler")
	}, upTimeout, time.Second, "the scheduler's output is the stub's, run from the supplied path")
	b, err := os.ReadFile(serial)
	require.NoError(t, err)
	assert.Contains(t, string(b), "service=gaon-apiserver", "the API server, with no binary supplied, runs the image's")
}
