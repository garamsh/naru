package node

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

func gaonCfg(t *testing.T, index int, hostPort int) Config {
	t.Helper()
	certs, err := pki.NewCluster([]string{"node-1", "node-2"}, net.ParseIP("10.78.0.1"), net.ParseIP("127.0.0.1"))
	require.NoError(t, err)
	c := cfg(index, 2, index == 1, false)
	c.Gaon = &Gaon{Certs: certs, APIServerIP: "10.78.0.1", HostPort: hostPort}
	return c
}

func serviceNamed(t *testing.T, n nodeConfig, name string) service {
	t.Helper()
	for _, s := range n.Spec.Services {
		if s.Name == name {
			return s
		}
	}
	require.Failf(t, "no such service", "%s in %v", name, names(n))
	return service{}
}

func TestNaruYAML_GaonRunsTheRegistryOnEveryNodeAndTheControlPlaneOnNodeOneInTheOrderEachDialsWhatIsBefore(t *testing.T) {
	first, second := parse(t, gaonCfg(t, 1, 6443)), parse(t, gaonCfg(t, 2, 0))

	want := []string{"mvmd", "registry", "etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"}
	assert.Equal(t, want, names(first))
	assert.Equal(t, want, names(second), "init, not the file, leaves the control plane out of Node 2")
	for _, n := range []nodeConfig{first, second} {
		for _, s := range n.Spec.Services {
			agentOrMvmd := s.Name == "gaon-node-agent" || s.Name == "mvmd" || s.Name == "registry"
			assert.Equal(t, agentOrMvmd, !s.ControlPlane, s.Name)
		}
	}
	assert.True(t, first.Spec.ControlPlane)
	assert.False(t, second.Spec.ControlPlane)
}

func TestNaruYAML_GaonServicesTakeFilesAndArgvAndNoEnvironment(t *testing.T) {
	n := parse(t, gaonCfg(t, 2, 0))

	agent := serviceNamed(t, n, "gaon-node-agent")
	assert.Equal(t, "/bin/gaon-node-agent", agent.Binary)
	assert.Equal(t, []string{
		"--api-server", "https://10.78.0.1:6443/apis/compute.gaon.garam.sh/v1alpha1", "--node-name", "node-2",
		"--mvmd-socket", MvmdSocket, "--tls-ca", "/run/gaon/ca.pem", "--tls-cert", "/run/gaon/agent.pem", "--tls-key", "/run/gaon/agent.key",
	}, agent.Args)
	for _, s := range n.Spec.Services {
		if s.Name != "mvmd" {
			assert.Empty(t, s.Env, s.Name)
		}
	}
	etcd := strings.Join(serviceNamed(t, n, "etcd").Args, " ")
	assert.Contains(t, etcd, "--data-dir "+DataMount+"/etcd", "etcd's data is on the data disk")
	assert.Contains(t, etcd, "--listen-client-urls http://127.0.0.1:2379", "etcd listens on the loopback only")
}

func TestNaruYAML_GaonCertificatesArriveAsFilesUnderRunWithKeysPrivate(t *testing.T) {
	files := func(c Config) map[string]string {
		m := map[string]string{}
		for _, f := range parse(t, c).Spec.Files {
			m[f.Path] = f.Mode
		}
		return m
	}
	first, second := files(gaonCfg(t, 1, 6443)), files(gaonCfg(t, 2, 0))

	for _, f := range []string{"ca.pem", "agent.pem", "agent.key"} {
		assert.Contains(t, first, "/run/gaon/"+f)
		assert.Contains(t, second, "/run/gaon/"+f)
	}
	for _, f := range []string{"apiserver", "scheduler", "controllers"} {
		assert.Contains(t, first, "/run/gaon/"+f+".pem")
		assert.Equal(t, "0600", first["/run/gaon/"+f+".key"])
		assert.NotContains(t, second, "/run/gaon/"+f+".key", "a Node without the control plane holds none of its keys")
	}
	assert.Equal(t, "0600", second["/run/gaon/agent.key"])
}

func TestNaruYAML_EachNodesAgentFileIsItsOwnCertificate(t *testing.T) {
	c1, c2 := gaonCfg(t, 1, 6443), gaonCfg(t, 2, 0)
	c2.Gaon.Certs = c1.Gaon.Certs // one cluster
	body := func(c Config) string {
		for _, f := range parse(t, c).Spec.Files {
			if f.Path == "/run/gaon/agent.pem" {
				return f.Content
			}
		}
		return ""
	}

	assert.Equal(t, string(c1.Gaon.Certs.Agents["node-1"].CertPEM), body(c1))
	assert.Equal(t, string(c1.Gaon.Certs.Agents["node-2"].CertPEM), body(c2))
	assert.NotEqual(t, body(c1), body(c2))
}

func TestNetworkConfig_OnlyTheNodeWithAHostPortHasASecondNIC(t *testing.T) {
	with, without := string(gaonCfg(t, 1, 6443).NetworkConfig()), string(gaonCfg(t, 2, 0).NetworkConfig())

	assert.Contains(t, with, "name: eth1")
	assert.Contains(t, with, "mac_address: '02:00:00:01:00:01'")
	assert.Contains(t, with, "address: '10.0.2.15'")
	assert.Equal(t, 1, strings.Count(with, "gateway:"), "the default route stays on the shared network")
	assert.NotContains(t, without, "eth1")
	assert.Equal(t, string(cfg(2, 2, false, false).NetworkConfig()), without, "a Node without it keeps the network-config the other tests judge")
}

func TestQemuArgs_TheHostNICForwardsLoopbackToTheAPIServer(t *testing.T) {
	spec := Spec{Config: gaonCfg(t, 1, 41234)}
	spec.Dir, spec.Disk, spec.Mcast = t.TempDir(), filepath.Join(t.TempDir(), "naru.img"), "230.78.0.1:5000"
	require.NoError(t, os.WriteFile(spec.Disk, []byte("disk"), 0o644))
	args, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)
	line := strings.Join(args, " ")

	assert.Contains(t, line, "-netdev user,id=net1,restrict=on,hostfwd=tcp:127.0.0.1:41234-10.0.2.15:6443")
	assert.Contains(t, line, "virtio-net-pci,netdev=net1,mac=02:00:00:01:00:01")
	assert.NotContains(t, argsOf(t, Spec{}), "net1", "a Node without a host port has one NIC")
}
