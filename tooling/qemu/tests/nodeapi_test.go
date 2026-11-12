//go:build qemu_real

package tests_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The tests of the Node API (ADR 0017, ADR 0018) boot one Node that runs gaon, with the user-data
// naructl writes from an authority made by `naructl gen api-ca`, and run the naructl built from the
// checkout on the host against node-1's API through the host forward. Each waits on a condition: the
// API answering, a service listed, a line on the console.

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// nodeAPI boots a one-Node gaon cluster with the Node API and waits for the Node to serve it.
func nodeAPI(t *testing.T) *node.Cluster {
	t.Helper()
	c := start(t, node.ClusterSpec{N: 1, Gaon: true, NodeAPI: true})
	require.NotNil(t, c.API)
	eventually(t, "the Node API answers an admin", func() bool { return ctl(c.API.Admin, "version").Err == nil })
	return c
}

// ctl runs the naructl of the checkout as the client file names, as an operator does: its first
// argument is the command, and the rest follow --config.
func ctl(file string, args ...string) (r result) {
	// `get config` is two words, and the flags follow both.
	n := 1
	if args[0] == "get" {
		n = 2
	}
	cmd := append(append(append([]string{}, args[:n]...), "--config", file), args[n:]...)
	res := tool.Run(cmd...)
	return result{Stdout: res.Stdout, Stderr: res.Stderr, Err: res.Err}
}

type result struct {
	Stdout, Stderr string
	Err            error
}

func (r result) refusal() string { return strings.TrimSpace(r.Stderr + r.Stdout) }

// versionOf reads the three lines `naructl version` prints.
func versionOf(t *testing.T, file string) (version, bootID string) {
	t.Helper()
	r := ctl(file, "version")
	require.NoError(t, r.Err, r.refusal())
	for _, line := range strings.Split(r.Stdout, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "version":
			version = f[1]
		case len(f) == 3 && f[0] == "boot" && f[1] == "ID":
			bootID = f[2]
		}
	}
	require.NotEmpty(t, version, r.Stdout)
	return version, bootID
}

// pids reads the NAME STATE PID columns of `naructl services`, by service name.
func pids(t *testing.T, file string) map[string]string {
	t.Helper()
	r := ctl(file, "services")
	require.NoError(t, r.Err, r.refusal())
	out := map[string]string{}
	for i, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 {
			continue
		}
		if f[1] == "running" {
			out[f[0]] = f[2]
		}
	}
	return out
}

// revisionOf is the revision `naructl get config` prints.
func revisionOf(t *testing.T, file string) int {
	t.Helper()
	r := ctl(file, "get", "config")
	require.NoError(t, r.Err, r.refusal())
	var got struct {
		Revision int `json:"revision"`
	}
	require.NoError(t, json.Unmarshal([]byte(r.Stdout), &got), r.Stdout)
	return got.Revision
}

// patchFile writes a merge patch the way an operator keeps one.
func patchFile(t *testing.T, patch string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "patch.yaml")
	require.NoError(t, os.WriteFile(p, []byte(patch), 0o600))
	return p
}

// idle is a patch that gives the Node a service that does nothing but run: the image's stand-in with
// no work to wait for. Its args differ per timeout.
func idle(timeout string) string {
	return "spec:\n  services:\n    - name: naru49-idle\n      binary: /bin/naru-standin\n      args: [\"-timeout\", \"" + timeout + "\"]\n"
}

func TestNode_NodeAPIAnswersVersionServicesAndLogsAndRefusesAForeignClientAndAReaderTheApply(t *testing.T) {
	c := nodeAPI(t)
	n := c.Nodes[0]
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION, the version the image was built with")
	top, err := tool.Topology()
	require.NoError(t, err)

	// The console says the API serves, and not that it is off (what make judge-serial -node-api checks).
	expectAll(t, n, judge.NodeAPI(true))

	version, bootID := versionOf(t, c.API.Admin)
	assert.Equal(t, want, version, "naru-init reports the version the image was built with")
	assert.Regexp(t, uuid, bootID)
	for _, role := range []string{c.API.Admin, c.API.Reader} {
		v, id := versionOf(t, role)
		assert.Equal(t, want, v)
		assert.Equal(t, bootID, id, "one boot, one ID, whoever asks")
	}

	eventually(t, "every service of node-1 is listed running", func() bool {
		running := pids(t, c.API.Reader)
		for _, s := range top.Running(true) {
			if running[s.Name] == "" {
				return false
			}
		}
		return true
	})

	logs := ctl(c.API.Reader, "logs", "etcd")
	require.NoError(t, logs.Err, logs.refusal())
	assert.NotEmpty(t, strings.TrimSpace(logs.Stdout), "etcd has logged by the time it is running")
	assert.Equal(t, 0, revisionOf(t, c.API.Reader), "the user-data is revision 0")

	// A client whose certificate is from another authority is refused, where the same file with the
	// authority's own certificate is not.
	foreign := ctl(c.API.Foreign, "version")
	require.Error(t, foreign.Err)
	assert.Contains(t, foreign.refusal(), "certificate", "the handshake refuses the foreign pair")
	assert.NoError(t, ctl(c.API.Admin, "version").Err)

	// A reader reads and cannot apply: the refusal is the Node's 403 and the revision stays.
	patch := patchFile(t, idle("1m"))
	denied := ctl(c.API.Reader, "apply", "-f", patch)
	require.Error(t, denied.Err)
	assert.Contains(t, denied.refusal(), "403")
	assert.Equal(t, 0, revisionOf(t, c.API.Admin), "a refused apply changes nothing")
	assert.NotContains(t, pids(t, c.API.Admin), "naru49-idle")
}

func TestNode_ANoRebootApplyAddsAServiceAndChangesItsArgsAndRestartsOnlyThatServiceWithoutRebooting(t *testing.T) {
	c := nodeAPI(t)
	admin := c.API.Admin
	_, bootID := versionOf(t, admin)
	before := pids(t, admin)
	require.NotEmpty(t, before)

	added := ctl(admin, "apply", "-f", patchFile(t, idle("1m")), "--mode", "no-reboot")
	require.NoError(t, added.Err, added.refusal())
	assert.Contains(t, added.Stdout, "applied revision 1")
	eventually(t, "the added service runs", func() bool { return pids(t, admin)["naru49-idle"] != "" })
	afterAdd := pids(t, admin)
	for name, pid := range before {
		assert.Equal(t, pid, afterAdd[name], "%s keeps its PID when a service is added", name)
	}

	changed := ctl(admin, "apply", "-f", patchFile(t, idle("2m")), "--mode", "no-reboot")
	require.NoError(t, changed.Err, changed.refusal())
	assert.Contains(t, changed.Stdout, "applied revision 2")
	eventually(t, "the changed service runs under a new PID", func() bool {
		pid := pids(t, admin)["naru49-idle"]
		return pid != "" && pid != afterAdd["naru49-idle"]
	})
	afterChange := pids(t, admin)
	for name, pid := range before {
		assert.Equal(t, pid, afterChange[name], "%s keeps its PID when another service's args change", name)
	}
	_, after := versionOf(t, admin)
	assert.Equal(t, bootID, after, "no reboot")
	assert.Equal(t, 2, revisionOf(t, admin))
}

func TestNode_ARefusedApplyLeavesTheRevisionAsItWasWhetherStaleInvalidOrNeedingAReboot(t *testing.T) {
	c := nodeAPI(t)
	admin := c.API.Admin
	require.NoError(t, ctl(admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	require.Equal(t, 1, revisionOf(t, admin))

	// 409: the revision the request names is no longer the Node's. naructl always names the current
	// one, so the request is made as the client file says, with the stale revision.
	status, body := rawApply(t, admin, 0, idle("3m"), "no-reboot")
	assert.Equal(t, http.StatusConflict, status, body)
	assert.Contains(t, body, `"revision":1`, "the answer names the stored revision")
	assert.Equal(t, 1, revisionOf(t, admin))

	// 422: a service name init refuses.
	invalid := ctl(admin, "apply", "-f", patchFile(t, "spec:\n  services:\n    - name: Not_A_Name\n      binary: /bin/naru-standin\n"))
	require.Error(t, invalid.Err)
	assert.Contains(t, invalid.refusal(), "422")
	assert.Contains(t, invalid.refusal(), "name")
	assert.Equal(t, 1, revisionOf(t, admin))

	// 422: a field only a reboot can change, under no-reboot.
	reboot := ctl(admin, "apply", "-f", patchFile(t, "spec:\n  api:\n    listen: \"0.0.0.0:50001\"\n"), "--mode", "no-reboot")
	require.Error(t, reboot.Err)
	assert.Contains(t, reboot.refusal(), "422")
	assert.Contains(t, reboot.refusal(), "spec.api.listen")
	assert.Equal(t, 1, revisionOf(t, admin))
}

func TestNode_AStagedApplyTakesEffectAfterTheRebootAtTheStoredRevision(t *testing.T) {
	c := nodeAPI(t)
	admin := c.API.Admin
	_, bootID := versionOf(t, admin)

	staged := ctl(admin, "apply", "-f", patchFile(t, idle("1m")), "--mode", "staged")
	require.NoError(t, staged.Err, staged.refusal())
	assert.Contains(t, staged.Stdout, "applied revision 1, mode staged")
	assert.Equal(t, 1, revisionOf(t, admin), "the change is stored")
	assert.NotContains(t, pids(t, admin), "naru49-idle", "and not yet in effect")

	require.NoError(t, c.Nodes[0].Reboot())
	eventually(t, "the Node serves its API after the reboot", func() bool { return ctl(admin, "version").Err == nil })
	_, after := versionOf(t, admin)
	assert.NotEqual(t, bootID, after, "a reboot is a new boot")
	eventually(t, "the staged service runs after the reboot", func() bool { return pids(t, admin)["naru49-idle"] != "" })
	assert.Equal(t, 1, revisionOf(t, admin), "the Node booted at the stored revision")
}

func TestNode_UserDataOfV030FormWithNoSpecAPIBootsWithItsServicesAndLogsThatTheAPIIsOff(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 1, Gaon: true})
	n := c.Nodes[0]
	top, err := tool.Topology()
	require.NoError(t, err)

	userData, err := os.ReadFile(filepath.Join(filepath.Dir(n.ConsolePath()), "naru.yaml"))
	require.NoError(t, err)
	var parsed struct {
		Spec map[string]any `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(userData, &parsed))
	require.NotEmpty(t, parsed.Spec["services"], "the user-data has its services")
	assert.NotContains(t, parsed.Spec, "api", "the form v0.3.0 wrote: no spec.api")

	expectAll(t, n, judge.NodeAPI(false))
	for _, s := range top.Running(true) {
		require.NoError(t, n.WaitFor(context.Background(), bootTimeout, "started", "service="+s.Name), s.Name)
	}
}

// rawApply posts a patch at an explicit revision to the Node API with the client file's pair, as
// naructl cannot: it always names the Node's current revision.
func rawApply(t *testing.T, file string, revision int, patchYAML, mode string) (int, string) {
	t.Helper()
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	var f struct {
		Endpoints            []string `yaml:"endpoints"`
		CA, Certificate, Key string
	}
	require.NoError(t, yaml.Unmarshal(b, &f))
	pair, err := tls.X509KeyPair([]byte(f.Certificate), []byte(f.Key))
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM([]byte(f.CA)))
	hc := &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}}}}
	var patch map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(patchYAML), &patch))
	payload, err := json.Marshal(map[string]any{"revision": revision, "patch": patch, "mode": mode})
	require.NoError(t, err)
	resp, err := hc.Post("https://"+f.Endpoints[0]+"/v1/config", "application/json", bytes.NewReader(payload))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, err = out.ReadFrom(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out.String()
}
