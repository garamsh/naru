package command_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// apiAuthority makes the Node API authority of a deployment and returns the cluster's directory.
func apiAuthority(t *testing.T, dep deployment) {
	t.Helper()
	mustRun(t, "gen", "api-ca", "--ca-dir", dep.ca)
}

type specAPI struct {
	Spec struct {
		API *struct {
			Listen      string
			ClientCA    string `yaml:"clientCA"`
			Certificate string
			Key         string
		}
	}
}

func apiOf(t *testing.T, path string) *struct {
	Listen      string
	ClientCA    string `yaml:"clientCA"`
	Certificate string
	Key         string
} {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var u specAPI
	require.NoError(t, yaml.Unmarshal(b, &u))
	return u.Spec.API
}

func TestGenAPICA_WritesTheAuthorityWithAPrivateKeyAndRefusesToReplaceIt(t *testing.T) {
	dir := t.TempDir()

	mustRun(t, "gen", "api-ca", "--ca-dir", dir)

	cert, err := os.Stat(filepath.Join(dir, "api", "ca.pem"))
	require.NoError(t, err)
	key, err := os.Stat(filepath.Join(dir, "api", "ca.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), cert.Mode().Perm())
	assert.Equal(t, os.FileMode(0o600), key.Mode().Perm(), "the authority's key is private")
	before, err := os.ReadFile(filepath.Join(dir, "api", "ca.key"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "ca.key"))
	assert.ErrorIs(t, err, os.ErrNotExist, "no gaon CA key was needed, and none was made")

	out, err := run(t, "gen", "api-ca", "--ca-dir", dir)
	require.Error(t, err, "a second gen api-ca would lose the first's key")
	assert.Contains(t, out, "already holds a Node API authority")
	after, err := os.ReadFile(filepath.Join(dir, "api", "ca.key"))
	require.NoError(t, err)
	assert.Equal(t, before, after, "the refused run left the key as it was")
}

func TestGenAPICA_IsIndependentOfGaonsCA(t *testing.T) {
	dep := newDeployment(t)

	apiAuthority(t, dep)

	gaon, err := os.ReadFile(filepath.Join(dep.ca, "ca.pem"))
	require.NoError(t, err)
	api, err := os.ReadFile(filepath.Join(dep.ca, "api", "ca.pem"))
	require.NoError(t, err)
	assert.NotEqual(t, gaon, api, "two authorities")
	out, err := run(t, "gen", "ca", "--out", dep.ca)
	require.Error(t, err)
	assert.Contains(t, out, "already holds an authority", "gen ca still sees its own key beside api/")
}

func TestGenNode_WithAnAPIAuthorityCarriesSpecAPIOfAPairItIssuedAndNeverTheAuthoritysKey(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)

	for name, extra := range map[string][]string{
		"worker":        nil,
		"control plane": {"--control-plane", "--serving-address", "10.1.1.1", "--serving-address", "127.0.0.1"},
	} {
		t.Run(name, func(t *testing.T) {
			out := dep.node(t, "node-2", extra...)
			a := apiOf(t, out)
			require.NotNil(t, a, "spec.api")

			apiCA, err := os.ReadFile(filepath.Join(dep.ca, "api", "ca.pem"))
			require.NoError(t, err)
			assert.Equal(t, string(apiCA), a.ClientCA, "clientCA is the authority's certificate")
			cert := certOf(t, a.Certificate)
			_, err = cert.Verify(x509.VerifyOptions{Roots: roots(t, filepath.Join(dep.ca, "api", "ca.pem")), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			require.NoError(t, err, "the serving pair is the API authority's")
			_, err = cert.Verify(x509.VerifyOptions{Roots: roots(t, filepath.Join(dep.ca, "ca.pem")), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			assert.Error(t, err, "and not gaon's CA's")
			var ips []string
			for _, ip := range cert.IPAddresses {
				ips = append(ips, ip.String())
			}
			assert.Contains(t, ips, "192.168.0.20", "the Node's address")
			assert.Contains(t, ips, "127.0.0.1")
			assert.Len(t, ips, len(slices.Compact(slices.Sorted(slices.Values(ips)))), "each address once")
			if len(extra) > 0 {
				assert.Contains(t, ips, "10.1.1.1", "a serving address")
			}

			key, err := os.ReadFile(filepath.Join(dep.ca, "api", "ca.key"))
			require.NoError(t, err)
			b, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.NotContains(t, string(b), strings.TrimSpace(string(key)), "no Node carries the authority's key")
			assert.NotContains(t, string(b), "listen:", "init's default listen")
			validate(t, out)
		})
	}
}

func TestGenNode_WithoutAnAPIAuthorityCarriesNoSpecAPI(t *testing.T) {
	dep := newDeployment(t)

	out := dep.node(t, "node-2")

	assert.Nil(t, apiOf(t, out))
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "api:", "the user-data is the one v0.3.0 reads")
	validate(t, out)
}

func TestGenNode_AnAPIDirectoryThatHoldsNoAuthorityIsRefused(t *testing.T) {
	dep := newDeployment(t)
	require.NoError(t, os.Mkdir(filepath.Join(dep.ca, "api"), 0o750))

	out, err := run(t, "gen", "node", "--ca", dep.ca, "--name", "node-2", "--address", "192.168.0.20/24", "--api-server", "192.168.0.10",
		"--machine-prefix", "10.200.2.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "n.yaml"))

	require.Error(t, err, "a half-made authority would silently leave the Node without an API")
	assert.Contains(t, out, "Node API authority")
}

func TestGenAPIClient_WritesAPrivateFileWhoseCertificateCarriesTheRolesOrganization(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)

	for role, org := range map[string]string{"admin": "naru:admin", "reader": "naru:reader"} {
		t.Run(role, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), role+".yaml")
			mustRun(t, "gen", "api-client", "--ca-dir", dep.ca, "--role", role, "--endpoint", "192.168.0.10", "--endpoint", "192.168.0.20:50001", "--out", out)

			st, err := os.Stat(out)
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(), "the file carries a key")
			b, err := os.ReadFile(out)
			require.NoError(t, err)
			var f struct {
				Endpoints            []string
				CA, Certificate, Key string
			}
			require.NoError(t, yaml.Unmarshal(b, &f))
			assert.Equal(t, []string{"192.168.0.10:50000", "192.168.0.20:50001"}, f.Endpoints, "the default port is added")
			apiCA, err := os.ReadFile(filepath.Join(dep.ca, "api", "ca.pem"))
			require.NoError(t, err)
			assert.Equal(t, string(apiCA), f.CA)
			cert := certOf(t, f.Certificate)
			assert.Equal(t, []string{org}, cert.Subject.Organization)
			assert.Equal(t, "naru-"+role, cert.Subject.CommonName)
			_, err = cert.Verify(x509.VerifyOptions{Roots: roots(t, filepath.Join(dep.ca, "api", "ca.pem")), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			assert.NoError(t, err)
			assert.Contains(t, f.Key, "PRIVATE KEY")
		})
	}
}

func TestGenAPIClient_RefusesWhatCannotMakeAUsableFile(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	out := filepath.Join(t.TempDir(), "c.yaml")
	args := func(extra ...string) []string {
		return append([]string{"gen", "api-client", "--ca-dir", dep.ca, "--role", "admin", "--endpoint", "192.168.0.10", "--out", out}, extra...)
	}
	mustRun(t, args()...) // control: the same flags succeed

	for name, bad := range map[string][]string{
		"unknown role":      {"--role", "root"},
		"a name, not an IP": {"--endpoint", "node-1.example"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(t, args(bad...)...)
			assert.Error(t, err)
		})
	}
	_, err := run(t, "gen", "api-client", "--ca-dir", dep.ca, "--role", "admin", "--out", out)
	assert.Error(t, err, "an endpoint is required")
	_, err = run(t, "gen", "api-client", "--ca-dir", t.TempDir(), "--role", "admin", "--endpoint", "192.168.0.10", "--out", out)
	assert.Error(t, err, "without gen api-ca there is no authority to issue from")
}

// fakeNode is an in-memory Node API: a TLS 1.3 server that requires a client certificate from the
// API authority and speaks the wire shapes of ADR 0017 and ADR 0018, using the serving pair that
// gen node put into a Node's user-data.
type fakeNode struct {
	srv *httptest.Server

	mu                sync.Mutex
	revision          int
	requests          []string
	posted            map[string]any
	postRev           float64
	postMode          string
	postDry           bool
	logQuery          string
	stalenessRevision int
}

func newFakeNode(t *testing.T, dep deployment) *fakeNode { return newFakeNodeTLS(t, dep, 0) }

// newFakeNodeTLS is a fake Node whose TLS stops at maxTLS, when it is not 0.
func newFakeNodeTLS(t *testing.T, dep deployment, maxTLS uint16) *fakeNode {
	t.Helper()
	spec := apiOf(t, dep.node(t, "node-2"))
	require.NotNil(t, spec)
	pair, err := tls.X509KeyPair([]byte(spec.Certificate), []byte(spec.Key))
	require.NoError(t, err)
	f := &fakeNode{revision: 4}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		writeJSON(w, 200, map[string]any{"version": "v9.9.9", "bootId": "boot-1", "uptimeSeconds": 125.5})
	})
	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		writeJSON(w, 200, map[string]any{"services": []map[string]any{
			{"name": "etcd", "state": "running", "pid": 42, "restarts": 1, "startedAt": "2026-11-13T01:02:03Z"},
			{"name": "registry", "state": "stopped", "pid": 0, "restarts": 0},
		}})
	})
	mux.HandleFunc("GET /v1/services/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.mu.Lock()
		f.logQuery = r.URL.RawQuery
		f.mu.Unlock()
		if r.PathValue("name") != "etcd" {
			writeJSON(w, 404, map[string]any{"error": `no service named "` + r.PathValue("name") + `"`})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "etcd: first\netcd: second\n")
	})
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"revision": f.revision, "config": map[string]any{
			"apiVersion": "naru.garam.sh/v1alpha1",
			"spec":       map[string]any{"api": map[string]any{"key": "REDACTED", "certificate": "CERT"}},
		}})
	})
	mux.HandleFunc("POST /v1/config", func(w http.ResponseWriter, r *http.Request) {
		f.note(r)
		var body struct {
			Revision float64        `json:"revision"`
			Patch    map[string]any `json:"patch"`
			Mode     string         `json:"mode"`
			DryRun   bool           `json:"dryRun"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posted, f.postRev, f.postMode, f.postDry = body.Patch, body.Revision, body.Mode, body.DryRun
		switch {
		case f.stalenessRevision != 0:
			writeJSON(w, 409, map[string]any{"error": "the request names revision 4 and the Node is at revision 5", "revision": f.stalenessRevision})
			return
		case body.Patch["invalid"] != nil:
			writeJSON(w, 422, map[string]any{"error": "spec.services[0].binary: must be an absolute path", "fields": []string{"spec.services[0].binary", "spec.files[1].path"}})
			return
		case body.Patch["failing"] != nil:
			writeJSON(w, 500, map[string]any{"error": "restart etcd failed after revision 5 was stored", "revision": 5})
			return
		}
		rev := int(body.Revision) + 1
		if body.DryRun {
			rev = int(body.Revision)
		}
		writeJSON(w, 200, map[string]any{"revision": rev, "mode": body.Mode, "dryRun": body.DryRun,
			"actions": []map[string]any{{"action": "store", "target": "config"}, {"action": "restart", "target": "etcd"}}})
	})
	// The handshake and the role come first, as they do in init: 403 for a certificate with no role
	// that allows the method.
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		need := "naru:reader"
		if r.Method != http.MethodGet {
			need = "naru:admin"
		}
		orgs := r.TLS.VerifiedChains[0][0].Subject.Organization
		if !slices.Contains(orgs, "naru:admin") && !slices.Contains(orgs, need) {
			writeJSON(w, 403, map[string]any{"error": "the client certificate's role does not allow this request"})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	f.srv.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   maxTLS,
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots(t, filepath.Join(dep.ca, "api", "ca.pem")),
	}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fakeNode) note(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
}

func (f *fakeNode) addr() string { return f.srv.Listener.Addr().(*net.TCPAddr).String() }

// clientFile makes a client file of the role for the fake Node.
func (f *fakeNode) clientFile(t *testing.T, dep deployment, role string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), role+".yaml")
	mustRun(t, "gen", "api-client", "--ca-dir", dep.ca, "--role", role, "--endpoint", f.addr(), "--out", out)
	return out
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "patch.yaml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestVersion_PrintsWhatTheNodeReports(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)

	out, err := run(t, "version", "--config", node.clientFile(t, dep, "reader"))

	require.NoError(t, err, out)
	assert.Contains(t, out, "v9.9.9")
	assert.Contains(t, out, "boot-1")
	assert.Contains(t, out, "2m5s", "the uptime")
	assert.Equal(t, []string{"GET /v1/version"}, node.requests)
}

func TestServices_PrintsEachServicesState(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)

	out, err := run(t, "services", "--config", node.clientFile(t, dep, "reader"), "--node", node.addr())

	require.NoError(t, err, out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 3)
	assert.Equal(t, []string{"NAME", "STATE", "PID", "RESTARTS", "STARTED"}, strings.Fields(lines[0]))
	assert.Equal(t, []string{"etcd", "running", "42", "1", "2026-11-13T01:02:03Z"}, strings.Fields(lines[1]))
	assert.Equal(t, []string{"registry", "stopped", "0", "0"}, strings.Fields(lines[2]))
}

func TestLogs_PrintsTheKeptLinesAndFollowAsksTheNodeToStream(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "reader")

	out, err := run(t, "logs", "etcd", "--config", cfg)
	require.NoError(t, err, out)
	assert.Equal(t, "etcd: first\netcd: second\n", out)
	assert.Empty(t, node.logQuery, "no follow without -f")

	out, err = run(t, "logs", "--config", cfg, "-f", "etcd")
	require.NoError(t, err, out)
	assert.Equal(t, "etcd: first\netcd: second\n", out)
	assert.Equal(t, "follow=true", node.logQuery)
}

func TestLogs_AnUnknownServiceIsOneLineAndExitCode1(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)

	out, err := run(t, "logs", "nope", "--config", node.clientFile(t, dep, "reader"))

	require.Error(t, err)
	assert.Contains(t, out, `404 Not Found: no service named "nope"`)
	_, err = run(t, "logs", "--config", node.clientFile(t, dep, "reader"))
	assert.Error(t, err, "a service name is required")
	_, err = run(t, "logs", "etcd", "registry", "--config", node.clientFile(t, dep, "reader"))
	assert.Error(t, err, "and only one")
}

func TestGetConfig_PrintsTheRevisionAndTheConfigurationAsTheNodeRedactedIt(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "reader")

	out, err := run(t, "get", "config", "--config", cfg)

	require.NoError(t, err, out)
	var got struct {
		Revision int
		Config   map[string]any
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, 4, got.Revision)
	assert.Contains(t, out, `"key": "REDACTED"`)
	file, err := os.ReadFile(cfg)
	require.NoError(t, err)
	var f struct{ Key string }
	require.NoError(t, yaml.Unmarshal(file, &f))
	assert.NotContains(t, out, strings.TrimSpace(f.Key), "the client's own key is never printed")
}

func TestApply_SendsThePatchAgainstTheRevisionItReadAndPrintsTheActions(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "admin")
	patch := writeFile(t, "spec:\n  services:\n    - name: etcd\n      args: [--a, --b]\n")

	out, err := run(t, "apply", "-f", patch, "--config", cfg)

	require.NoError(t, err, out)
	assert.Equal(t, []string{"GET /v1/config", "POST /v1/config"}, node.requests, "the revision is read first")
	assert.EqualValues(t, 4, node.postRev, "the patch is against the revision read")
	assert.Equal(t, "no-reboot", node.postMode, "the default mode")
	assert.False(t, node.postDry)
	assert.Equal(t, map[string]any{"spec": map[string]any{"services": []any{map[string]any{"name": "etcd", "args": []any{"--a", "--b"}}}}}, node.posted)
	assert.Contains(t, out, "applied revision 5, mode no-reboot")
	assert.Contains(t, out, "  store config\n  restart etcd\n")
}

func TestApply_TakesTheModeAndDryRunAndAJSONPatch(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "admin")

	out, err := run(t, "apply", "-f", writeFile(t, `{"spec": {"controlPlane": true}}`), "--config", cfg, "--mode", "staged", "--dry-run")

	require.NoError(t, err, out)
	assert.Equal(t, "staged", node.postMode)
	assert.True(t, node.postDry)
	assert.Equal(t, map[string]any{"spec": map[string]any{"controlPlane": true}}, node.posted)
	assert.Contains(t, out, "dry run at revision 4, mode staged: nothing was done")
}

func TestApply_ARefusalIsOneLineWithTheNodesReasonAndExitCode1(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	admin := node.clientFile(t, dep, "admin")

	// control: a patch the Node accepts, with the same client
	_, err := run(t, "apply", "-f", writeFile(t, "ok: true"), "--config", admin)
	require.NoError(t, err)

	t.Run("422 names the fields", func(t *testing.T) {
		out, err := run(t, "apply", "-f", writeFile(t, "invalid: true"), "--config", admin)
		require.Error(t, err)
		assert.Equal(t, "422 Unprocessable Entity: spec.services[0].binary: must be an absolute path (fields: spec.services[0].binary, spec.files[1].path)", out)
	})
	t.Run("409 names the current revision", func(t *testing.T) {
		node.mu.Lock()
		node.stalenessRevision = 5
		node.mu.Unlock()
		out, err := run(t, "apply", "-f", writeFile(t, "ok: true"), "--config", admin)
		require.Error(t, err)
		assert.Equal(t, "409 Conflict: the request names revision 4 and the Node is at revision 5", out)
		node.mu.Lock()
		node.stalenessRevision = 0
		node.mu.Unlock()
	})
	t.Run("403 for a reader", func(t *testing.T) {
		out, err := run(t, "apply", "-f", writeFile(t, "ok: true"), "--config", node.clientFile(t, dep, "reader"))
		require.Error(t, err)
		assert.Equal(t, "403 Forbidden: the client certificate's role does not allow this request", out)
	})
	t.Run("500 is one line too", func(t *testing.T) {
		out, err := run(t, "apply", "-f", writeFile(t, "failing: true"), "--config", admin)
		require.Error(t, err)
		assert.Equal(t, "500 Internal Server Error: restart etcd failed after revision 5 was stored", out)
	})
}

func TestApply_RefusesWhatItCannotSendBeforeAnyRequest(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	cfg := node.clientFile(t, dep, "admin")
	patch := writeFile(t, "ok: true")

	for name, args := range map[string][]string{
		"an unknown mode":           {"apply", "-f", patch, "--config", cfg, "--mode", "reboot"},
		"no patch":                  {"apply", "--config", cfg},
		"a patch file that is none": {"apply", "-f", filepath.Join(t.TempDir(), "missing"), "--config", cfg},
		"a patch that is a list":    {"apply", "-f", writeFile(t, "- a\n- b\n"), "--config", cfg},
		"a patch that is empty":     {"apply", "-f", writeFile(t, ""), "--config", cfg},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(t, args...)
			assert.Error(t, err)
			assert.NotContains(t, node.requests, "POST /v1/config")
		})
	}
}

func TestClientCommands_NeedAClientFileTheNodeTrustsAndAKnownNode(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	good := node.clientFile(t, dep, "reader")
	_, err := run(t, "version", "--config", good)
	require.NoError(t, err, "control: the file of the Node's own authority works")

	other := newDeployment(t)
	apiAuthority(t, other)
	foreign := node.clientFile(t, other, "admin")

	for name, args := range map[string][]string{
		"no --config":                          {"version"},
		"a client file from another authority": {"version", "--config", foreign},
		"a missing client file":                {"version", "--config", filepath.Join(t.TempDir(), "none")},
		"a node that is a name":                {"version", "--config", good, "--node", "node-1"},
		"a Node nothing listens on":            {"version", "--config", good, "--node", "127.0.0.1:1"},
		"extra arguments":                      {"version", "--config", good, "extra"},
		"an unknown get":                       {"get", "nodes", "--config", good},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(t, args...)
			assert.Error(t, err)
		})
	}
}

func TestClientCommands_AMalformedClientFileIsReportedWithoutItsText(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	file, err := os.ReadFile(node.clientFile(t, dep, "admin"))
	require.NoError(t, err)
	var f struct{ Key string }
	require.NoError(t, yaml.Unmarshal(file, &f))
	// The key where a list belongs: a parser's type error would quote it.
	bad := writeFile(t, "endpoints: "+yamlQuote(f.Key)+"\nca: x\ncertificate: x\nkey: x\n")

	out, err := run(t, "version", "--config", bad)

	require.Error(t, err)
	assert.Contains(t, out, "is not a client file")
	assert.NotContains(t, out, "PRIVATE KEY")
	assert.NotContains(t, out, strings.TrimSpace(strings.Split(f.Key, "\n")[1]), "no line of the key")
}

func yamlQuote(s string) string {
	b, _ := yaml.Marshal(s)
	return string(b)
}

func TestClientCommands_ANodeThatStopsAtTLS12IsRefused(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	current := newFakeNodeTLS(t, dep, tls.VersionTLS13)
	old := newFakeNodeTLS(t, dep, tls.VersionTLS12)

	_, err := run(t, "version", "--config", current.clientFile(t, dep, "reader"))
	require.NoError(t, err, "control: the same Node over TLS 1.3 is served")
	out, err := run(t, "version", "--config", old.clientFile(t, dep, "reader"))

	require.Error(t, err, out)
	assert.Empty(t, old.requests, "no request crossed a TLS 1.2 connection")
}
