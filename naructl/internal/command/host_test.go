package command_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/pki"
)

func TestGenHostCA_WritesTheAuthorityUnderHostAndRefusesToReplaceIt(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)

	key, err := os.Stat(filepath.Join(dep.ca, "host", "ca.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), key.Mode().Perm())
	_, err = os.Stat(filepath.Join(dep.ca, "host", "ca.pem"))
	require.NoError(t, err)

	out, err := run(t, "gen", "host-ca", "--ca-dir", dep.ca)
	assert.Error(t, err, "a second authority would replace the first")
	assert.Contains(t, out, "already holds")
}

func TestGenHostClient_TheRoleIsTheOrganizationTheHostAuthoritySigns(t *testing.T) {
	dep := newDeployment(t)
	_, err := run(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", "reader", "--endpoint", "192.168.0.10", "--out", filepath.Join(t.TempDir(), "c.yaml"))
	assert.Error(t, err, "no host authority yet")

	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	for role, org := range map[string]string{"provisioner": "naru:host-provisioner", "reader": "naru:host-reader"} {
		out := filepath.Join(t.TempDir(), "c.yaml")
		mustRun(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", role, "--endpoint", "192.168.0.10", "--out", out)
		var f struct {
			Endpoints   []string
			Certificate string
		}
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		require.NoError(t, yaml.Unmarshal(b, &f))
		assert.Equal(t, []string{"192.168.0.10:7443"}, f.Endpoints)
		assert.Equal(t, []string{org}, certOf(t, f.Certificate).Subject.Organization)
		st, _ := os.Stat(out)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}
	_, err = run(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", "admin", "--endpoint", "192.168.0.10", "--out", filepath.Join(t.TempDir(), "c.yaml"))
	assert.Error(t, err, "admin is a Node API role")
}

func TestGenNode_HostFlagsGiveSpecHostAndAServingPairWithoutTheAuthorityKey(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	hostKey, err := os.ReadFile(filepath.Join(dep.ca, "host", "ca.key"))
	require.NoError(t, err)

	path := dep.node(t, "node-1", "--host-region", "eu-1", "--host-label", "disk=ssd", "--host-label", "tier=gold")
	assert.Contains(t, validate(t, path), "", "naru-init accepts it")
	u := parse(t, path)
	for _, f := range []string{"tls.crt", "tls.key", "client-ca.crt"} {
		_, ok := u.file("/run/naru-hostd/" + f)
		assert.True(t, ok, f)
	}
	assert.Contains(t, u.names(), "naru-hostd")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(b), "region: eu-1")
	for _, f := range u.Spec.Files {
		assert.NotContains(t, f.Content, strings.TrimSpace(string(hostKey)), "the authority's key is not carried in %s", f.Path)
	}

	plain := parse(t, dep.node(t, "node-2"))
	assert.NotContains(t, plain.names(), "naru-hostd", "the control: without the flags there is no host API")
	_, ok := plain.file("/run/naru-hostd/tls.crt")
	assert.False(t, ok)
}

func TestGenNode_HostLabelsNeedARegionAndTheAuthority(t *testing.T) {
	dep := newDeployment(t)
	out := filepath.Join(t.TempDir(), "n.yaml")
	args := func(extra ...string) []string {
		return append([]string{
			"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24",
			"--api-server", "192.168.0.10", "--machine-prefix", "10.200.1.0/24",
			"--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", out,
		}, extra...)
	}
	_, err := run(t, args("--host-label", "a=b")...)
	assert.Error(t, err, "a label without a region")
	_, err = run(t, args("--host-region", "eu-1")...)
	assert.Error(t, err, "no host authority was generated")
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	_, err = run(t, args("--host-region", "eu-1", "--host-label", "novalue")...)
	assert.Error(t, err, "a label is key=value")
	mustRun(t, args("--host-region", "eu-1", "--host-label", "a=b")...)
}

// hostServer serves canned answers over mutual TLS from the host authority of dep, and returns its
// address.
func hostServer(t *testing.T, dep deployment, h http.Handler) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dep.ca, "host", "ca.pem"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(b))
	// A throwaway serving pair from the same authority, as a Node's.
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13}
	cert := servingPair(t, dep, net.ParseIP("127.0.0.1"))
	srv.TLS.Certificates = []tls.Certificate{cert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestMicrovm_CreateGetListDeleteSpeakTheHostAPIOverMutualTLS(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	var got map[string]any
	mux := http.NewServeMux()
	vm := map[string]any{"name": "web", "machineId": "web", "state": "running", "addresses": []string{"10.200.1.2"}, "cpu": 2, "memory": 1 << 28}
	mux.HandleFunc("POST /v1/microvms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(vm)
	})
	mux.HandleFunc("GET /v1/microvms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"microvms": []any{vm}})
	})
	mux.HandleFunc("GET /v1/microvms/web", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(vm) })
	mux.HandleFunc("POST /v1/microvms/web/stop", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "web", "state": "stopped"})
	})
	mux.HandleFunc("POST /v1/microvms/web/start", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "web", "state": "running"})
	})
	mux.HandleFunc("DELETE /v1/microvms/web", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/host", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"region": "eu-1", "labels": map[string]string{"disk": "ssd"}})
	})
	addr := hostServer(t, dep, mux)
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	mustRun(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", "provisioner", "--endpoint", addr, "--out", cfg)

	out, err := run(t, "microvm", "create", "--config", cfg, "--name", "web", "--image", "r.example/app@sha256:"+d("1"), "--cpu", "2", "--disk", "data=1024,ro")
	require.NoError(t, err, out)
	assert.Contains(t, out, "created web")
	assert.EqualValues(t, "web", got["name"])
	assert.EqualValues(t, 2, got["cpu"])
	assert.Equal(t, []any{map[string]any{"name": "data", "sizeBytes": float64(1024), "readOnly": true}}, got["disks"])

	out, err = run(t, "microvm", "list", "--config", cfg)
	require.NoError(t, err, out)
	assert.Contains(t, out, "10.200.1.2")
	out, err = run(t, "microvm", "get", "--config", cfg, "web")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"state": "running"`)
	out, err = run(t, "microvm", "stop", "--config", cfg, "web")
	require.NoError(t, err, out)
	assert.Contains(t, out, "stop web: stopped")
	out, err = run(t, "microvm", "start", "--config", cfg, "web")
	require.NoError(t, err, out)
	assert.Contains(t, out, "start web: running")
	out, err = run(t, "microvm", "delete", "--config", cfg, "web")
	require.NoError(t, err, out)
	assert.Contains(t, out, "deleted web")
	out, err = run(t, "host", "--config", cfg)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"region": "eu-1"`)

	_, err = run(t, "microvm", "create", "--config", cfg, "--name", "web", "--image", "x", "-f", "r.json")
	assert.Error(t, err, "-f and the flags do not mix")
	_, err = run(t, "microvm", "create", "--config", cfg, "--name", "web", "--image", "x", "--disk", "bad")
	assert.Error(t, err)
}

// servingPair is a serving pair of the host authority of dep for ip.
func servingPair(t *testing.T, dep deployment, ip net.IP) tls.Certificate {
	t.Helper()
	ca, err := pki.Load(filepath.Join(dep.ca, "host"))
	require.NoError(t, err)
	p, err := ca.Server("test", ip)
	require.NoError(t, err)
	c, err := tls.X509KeyPair(p.CertPEM, p.KeyPEM)
	require.NoError(t, err)
	return c
}

func TestMicrovmCreate_TheOSFormFlagsSendTheBootSetAndTheSeedAndRefuseTheImageFormsFlags(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/microvms", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "vm", "state": "running"})
	})
	addr := hostServer(t, dep, mux)
	cfg := filepath.Join(t.TempDir(), "c.yaml")
	mustRun(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", "provisioner", "--endpoint", addr, "--out", cfg)
	seed := filepath.Join(t.TempDir(), "user-data")
	require.NoError(t, os.WriteFile(seed, []byte("#cloud-config\n"), 0o600))

	out, err := run(t, "microvm", "create", "--config", cfg, "--name", "vm", "--boot-set", "alpine", "--cmdline", "quiet",
		"--seed", seed, "--data-disk", "4096", "--pod-prefix", "10.9.0.0/24")
	require.NoError(t, err, out)
	assert.EqualValues(t, "alpine", got["bootSet"])
	assert.EqualValues(t, "quiet", got["cmdline"])
	assert.EqualValues(t, "I2Nsb3VkLWNvbmZpZwo=", got["seed"], "the user-data in base64")
	assert.EqualValues(t, 4096, got["dataDisk"])
	assert.EqualValues(t, "10.9.0.0/24", got["podPrefix"])
	assert.NotContains(t, got, "image")

	_, err = run(t, "microvm", "create", "--config", cfg, "--name", "vm", "--boot-set", "alpine", "--seed", seed, "--data-disk", "4096", "--image", "x")
	assert.Error(t, err, "an image and a boot set do not mix")
	_, err = run(t, "microvm", "create", "--config", cfg, "--name", "vm", "--boot-set", "alpine", "--data-disk", "4096")
	assert.Error(t, err, "the OS form needs a seed")
}
