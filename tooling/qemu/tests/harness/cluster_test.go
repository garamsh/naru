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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
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
	cmd    *exec.Cmd
	dir    string
	log    string
	exited chan struct{}
	file   clusterfile.File
}

// launch runs naru-cluster with args from a directory outside the checkout, on a -dir of its own,
// and stops it when the test ends, which must leave no QEMU process of that -dir behind.
func launch(t *testing.T, args ...string) *running {
	t.Helper()
	r := &running{dir: filepath.Join(t.TempDir(), "cluster"), log: filepath.Join(t.TempDir(), "naru-cluster.log"), exited: make(chan struct{})}
	logFile, err := os.Create(r.log)
	require.NoError(t, err)
	absRelease, err := filepath.Abs(releaseDir)
	require.NoError(t, err)
	r.cmd = exec.Command(naruCluster, append([]string{"-release", absRelease, "-dir", r.dir}, args...)...) //nolint:gosec // the command just built
	r.cmd.Dir = t.TempDir()                                                                                // not the checkout
	r.cmd.Stdout, r.cmd.Stderr = logFile, logFile
	require.NoError(t, r.cmd.Start())
	go func() { _ = r.cmd.Wait(); close(r.exited) }()
	t.Cleanup(func() {
		_ = r.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-r.exited:
		case <-time.After(time.Minute):
			_ = r.cmd.Process.Kill()
			t.Errorf("naru-cluster did not stop within a minute of SIGTERM")
		}
		for _, pid := range qemuUnder(r.dir) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("the QEMU process %d outlived naru-cluster", pid)
		}
		t.Logf("naru-cluster log:\n%s", r.logText())
	})
	return r
}

// start is launch and then the wait for cluster.json, which appears once every Node is up.
func start(t *testing.T, args ...string) *running {
	t.Helper()
	r := launch(t, args...)
	require.NoError(t, r.waitClusterFile(upTimeout))
	return r
}

func (r *running) logText() string {
	b, _ := os.ReadFile(r.log)
	return string(b)
}

// waitClusterFile reads cluster.json once it appears. It fails, with the command's log, as soon as
// the command exits without having written it, and when the timeout passes.
func (r *running) waitClusterFile(timeout time.Duration) error {
	path := filepath.Join(r.dir, clusterfile.Name)
	deadline := time.After(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			var err error
			r.file, err = clusterfile.Read(path)
			return err
		}
		select {
		case <-r.exited:
			return fmt.Errorf("naru-cluster exited (%v) before cluster.json appeared; its log:\n%s", r.cmd.ProcessState, r.logText())
		case <-deadline:
			return fmt.Errorf("no cluster.json within %s; the log:\n%s", timeout, r.logText())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// alive reports whether the process runs; a zombie waiting to be reaped does not.
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndex(string(b), ") ")
	return i >= 0 && len(b) > i+2 && b[i+2] != 'Z'
}

// qemuUnder is the pids of the QEMU processes running on files under dir.
func qemuUnder(dir string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !alive(pid) {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if strings.Contains(string(cmdline), "qemu-system") && strings.Contains(string(cmdline), dir) {
			pids = append(pids, pid)
		}
	}
	return pids
}

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

	// A kill of the Node that is not the control plane is a power-off: logged, not fatal, and the
	// control plane keeps serving. The Nodes are told apart by their role, not their position.
	var victim, keeper clusterfile.Node
	for _, n := range f.Nodes {
		if n.ControlPlane {
			keeper = n
		} else {
			victim = n
		}
	}
	require.NotEmpty(t, victim.Name, "a Node that is not the control plane")
	require.NotEmpty(t, keeper.Name, "the control plane")
	require.NoError(t, syscall.Kill(victim.PID, syscall.SIGKILL))
	require.Eventually(t, func() bool { return !alive(victim.PID) }, time.Minute, 100*time.Millisecond, victim.Name+"'s process is gone")
	require.Eventually(t, func() bool {
		b := r.logText()
		return strings.Contains(b, "node lost") && strings.Contains(b, victim.Name)
	}, time.Minute, 100*time.Millisecond, "naru-cluster logs the loss")
	assert.True(t, alive(r.cmd.Process.Pid), "naru-cluster keeps running")
	assert.True(t, alive(keeper.PID), "the control plane keeps running")
	got, err := api.Node(ctx, keeper.Name)
	require.NoError(t, err, "the control plane's API server still answers")
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

	require.NotEmpty(t, f.Images)
	assert.Regexp(t, imageRef, f.Images[0].Reference, "images are listed whatever is disabled")
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

// TestCluster_ANodeStartedIsKilledWithNaruClusterEvenWhenItIsKilled shows that SIGKILL of naru-cluster,
// which it cannot catch, still leaves no Node's QEMU process running.
func TestCluster_ANodeStartedIsKilledWithNaruClusterEvenWhenItIsKilled(t *testing.T) {
	r := start(t, "-n", "2", "-gaon-bin", gaonDir, "-boot-pki=false")
	pids := []int{}
	for _, n := range r.file.Nodes {
		require.True(t, alive(n.PID), n.Name)
		pids = append(pids, n.PID)
	}

	require.NoError(t, r.cmd.Process.Signal(syscall.SIGKILL))

	<-r.exited
	for _, pid := range pids {
		require.Eventually(t, func() bool { return !alive(pid) }, 30*time.Second, 100*time.Millisecond, "the QEMU process %d is gone with naru-cluster", pid)
	}
	assert.Empty(t, qemuUnder(r.dir))
}

// TestCluster_ASignalDuringStartupStopsEveryNodeStartedAndSaysSo signals naru-cluster at two points
// before cluster.json exists, and shows exit status 3, the one-line reason, and no QEMU left.
func TestCluster_ASignalDuringStartupStopsEveryNodeStartedAndSaysSo(t *testing.T) {
	for name, trigger := range map[string]func(r *running) bool{
		"while the first Node is being started": func(r *running) bool {
			_, err := os.Stat(filepath.Join(r.dir, "node-1", "qemu.log"))
			return err == nil
		},
		"once the Nodes are started and being waited for": func(r *running) bool {
			return strings.Contains(r.logText(), "node started")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := launch(t, "-n", "2", "-gaon-bin", gaonDir, "-boot-pki=false")
			require.Eventually(t, func() bool { return trigger(r) }, time.Minute, 5*time.Millisecond, "the trigger happens")

			require.NoError(t, r.cmd.Process.Signal(syscall.SIGTERM))

			select {
			case <-r.exited:
			case <-time.After(time.Minute):
				t.Fatalf("naru-cluster did not stop within a minute of SIGTERM; its log:\n%s", r.logText())
			}
			assert.Equal(t, 3, r.cmd.ProcessState.ExitCode(), r.logText())
			assert.Contains(t, r.logText(), "signalled before every Node was up")
			assert.NoFileExists(t, filepath.Join(r.dir, clusterfile.Name))
			assert.Empty(t, qemuUnder(r.dir), "no QEMU process it started survives")
		})
	}
}

// TestCluster_ACommandThatExitsBeforeClusterJSONIsAFailureThatQuotesItsLog shows that the wait
// returns the command's failure at once instead of waiting out its timeout.
func TestCluster_ACommandThatExitsBeforeClusterJSONIsAFailureThatQuotesItsLog(t *testing.T) {
	r := launch(t, "-n", "0")
	began := time.Now()

	err := r.waitClusterFile(time.Minute)

	require.Error(t, err)
	assert.Less(t, time.Since(began), 30*time.Second, "it does not wait for its timeout")
	assert.Contains(t, err.Error(), "exited")
	assert.Contains(t, err.Error(), "-n is 0")
}
