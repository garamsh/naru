package layout_test

import (
	"crypto/x509"
	"encoding/pem"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/layout"
	"github.com/garamsh/naru/naructl/internal/pki"
)

// kubeSpec is the part of a Kubernetes Node's user-data the tests read.
type kubeSpec struct {
	Spec struct {
		ControlPlane bool `yaml:"controlPlane"`
		DataDisk     struct {
			Label       string `yaml:"label"`
			FormatBlank bool   `yaml:"formatBlank"`
		} `yaml:"dataDisk"`
		Network struct {
			Routes []struct {
				To  string `yaml:"to"`
				Via string `yaml:"via"`
			} `yaml:"routes"`
		} `yaml:"network"`
		Services []struct {
			Name         string   `yaml:"name"`
			Args         []string `yaml:"args"`
			ControlPlane bool     `yaml:"controlPlane"`
		} `yaml:"services"`
		Files []struct {
			Path    string `yaml:"path"`
			Content string `yaml:"content"`
		} `yaml:"files"`
		PKI struct {
			Authorities []struct {
				Name, Key string
			} `yaml:"authorities"`
			Certificates []struct {
				Name         string   `yaml:"name"`
				Authority    string   `yaml:"authority"`
				CommonName   string   `yaml:"commonName"`
				Organization string   `yaml:"organization"`
				Usage        string   `yaml:"usage"`
				DNSNames     []string `yaml:"dnsNames"`
				IPAddresses  []string `yaml:"ipAddresses"`
				Restart      []string `yaml:"restart"`
			} `yaml:"certificates"`
		} `yaml:"pki"`
	} `yaml:"spec"`
}

func kubeNode(t *testing.T, controlPlane bool, change func(*layout.Node)) layout.Node {
	t.Helper()
	ca, err := pki.New("test")
	require.NoError(t, err)
	k := &layout.KubeNode{
		ControlPlane: controlPlane,
		PodNetwork:   netip.MustParsePrefix("10.64.0.0/12"),
		PodPrefix:    netip.MustParsePrefix("10.64.2.0/24"),
		Peers:        []layout.KubePeer{{Address: netip.MustParseAddr("192.168.0.10"), PodPrefix: netip.MustParsePrefix("10.64.1.0/24")}},
	}
	if controlPlane {
		k.PodPrefix = netip.MustParsePrefix("10.64.1.0/24")
		k.Peers = []layout.KubePeer{{Address: netip.MustParseAddr("192.168.0.11"), PodPrefix: netip.MustParsePrefix("10.64.2.0/24")}}
		k.FrontProxy, err = pki.New("front")
		require.NoError(t, err)
		k.ServiceAccountKey, k.ServiceAccountPub = []byte("key"), []byte("pub")
	}
	n := layout.Node{
		Name: "node-2", Address: netip.MustParsePrefix("192.168.0.11/24"), APIServer: netip.MustParseAddr("192.168.0.10"),
		CA: ca, Kube: k,
	}
	if controlPlane {
		n.Name, n.Address = "node-1", netip.MustParsePrefix("192.168.0.10/24")
	}
	if change != nil {
		change(&n)
	}
	return n
}

func kubeUserData(t *testing.T, n layout.Node) kubeSpec {
	t.Helper()
	b, err := n.UserData()
	require.NoError(t, err)
	var s kubeSpec
	require.NoError(t, yaml.Unmarshal(b, &s))
	return s
}

func serviceNames(s kubeSpec) []string {
	var names []string
	for _, sv := range s.Spec.Services {
		names = append(names, sv.Name)
	}
	return names
}

func TestKubernetesUserData_WorkerRunsTheKubeletAndTheProxyAndNoControlPlane(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, false, nil))

	var running []string
	for _, sv := range s.Spec.Services {
		assert.Equal(t, layout.Kubernetes.OnlyControlPlane(sv.Name), sv.ControlPlane, "%s is marked for the control-plane Node as the topology has it", sv.Name)
		if !sv.ControlPlane {
			running = append(running, sv.Name)
		}
	}
	assert.Equal(t, []string{"containerd", "kubelet", "kube-proxy"}, running, "what a worker runs")
}

func TestKubernetesUserData_ControlPlaneRunsEveryServiceInTheTopologysOrder(t *testing.T) {
	n := kubeNode(t, true, func(n *layout.Node) { n.Kube.LocalRegistry = true })

	s := kubeUserData(t, n)

	assert.Equal(t, layout.Kubernetes.Names(), serviceNames(s))
	assert.True(t, s.Spec.ControlPlane)
}

func TestKubernetesUserData_AsksInitToFormatABlankDataDiskForAMicroVMWhoseHostGivesItOneUnformatted(t *testing.T) {
	for _, controlPlane := range []bool{true, false} {
		s := kubeUserData(t, kubeNode(t, controlPlane, nil))

		assert.Equal(t, layout.DataLabel, s.Spec.DataDisk.Label)
		assert.True(t, s.Spec.DataDisk.FormatBlank, "a microVM's data disk is blank at its first boot, and init refuses the boot without a labelled one")
	}
}

func TestKubernetesUserData_RoutesToEveryPeersPodPrefix(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, false, nil))

	require.Len(t, s.Spec.Network.Routes, 1)
	assert.Equal(t, "10.64.1.0/24", s.Spec.Network.Routes[0].To)
	assert.Equal(t, "192.168.0.10", s.Spec.Network.Routes[0].Via)
}

func TestKubernetesUserData_DisabledServiceIsLeftOutWithItsRestarts(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, func(n *layout.Node) { n.Disable = []string{"kubelet"} }))

	assert.NotContains(t, serviceNames(s), "kubelet")
	for _, c := range s.Spec.PKI.Certificates {
		assert.NotContains(t, c.Restart, "kubelet", c.Name)
	}
}

func TestKubernetesUserData_ControlPlaneIssuesItsKubeletTheIdentityTheNodeAuthorizerNames(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, nil))

	var client, serving bool
	for _, c := range s.Spec.PKI.Certificates {
		switch c.Name {
		case "kubelet-client":
			client = true
			assert.Equal(t, "system:node:node-1", c.CommonName)
			assert.Equal(t, "system:nodes", c.Organization)
			assert.Equal(t, "client", c.Usage)
		case "kubelet":
			serving = true
			assert.Equal(t, "server", c.Usage)
			assert.Equal(t, []string{"node-1"}, c.DNSNames)
		}
	}
	assert.True(t, client && serving)
}

func TestKubernetesUserData_WorkerCarriesItsPairsAsFilesAndNeverTheAuthoritysKey(t *testing.T) {
	n := kubeNode(t, false, nil)

	b, err := n.UserData()
	require.NoError(t, err)
	var s kubeSpec
	require.NoError(t, yaml.Unmarshal(b, &s))

	assert.Empty(t, s.Spec.PKI.Authorities, "a worker has no spec.pki to issue from")
	assert.NotContains(t, string(b), string(n.CA.KeyPEM), "the cluster CA's key is not in a worker's user-data")
	files := map[string]string{}
	for _, f := range s.Spec.Files {
		files[f.Path] = f.Content
	}
	assert.Equal(t, string(n.CA.CertPEM), files["/run/kubernetes/pki/ca.crt"])
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(n.CA.CertPEM))
	for name, want := range map[string]struct {
		cn, org string
		usage   x509.ExtKeyUsage
	}{
		"kubelet-client": {"system:node:node-2", "system:nodes", x509.ExtKeyUsageClientAuth},
		"kubelet":        {"node-2", "", x509.ExtKeyUsageServerAuth},
		"kube-proxy":     {"system:kube-proxy", "", x509.ExtKeyUsageClientAuth},
	} {
		block, _ := pem.Decode([]byte(files["/run/kubernetes/pki/"+name+".crt"]))
		require.NotNil(t, block, name)
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		_, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{want.usage}})
		assert.NoError(t, err, "%s is signed by the cluster CA for its usage", name)
		assert.Equal(t, want.cn, cert.Subject.CommonName)
		if want.org != "" {
			assert.Equal(t, []string{want.org}, cert.Subject.Organization)
		}
		assert.NotEmpty(t, files["/run/kubernetes/pki/"+name+".key"], name)
	}
}

func TestKubernetesUserData_ControlPlaneIssuesTheAPIServersPairsAndCarriesTheServiceAccountKeys(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, nil))

	pairs := map[string]string{}
	for _, c := range s.Spec.PKI.Certificates {
		pairs[c.Name] = c.Authority
		if c.Name == "apiserver" {
			assert.Contains(t, c.IPAddresses, "192.168.0.10")
			assert.Contains(t, c.IPAddresses, "10.96.0.1", "the API server's Service")
			assert.Contains(t, c.DNSNames, "kubernetes.default.svc.cluster.local")
		}
	}
	assert.Equal(t, "front-proxy", pairs["front-proxy-client"])
	assert.Equal(t, "etcd", pairs["apiserver-etcd"])
	assert.Equal(t, "cluster", pairs["controller-manager"])
	paths := map[string]string{}
	for _, f := range s.Spec.Files {
		paths[f.Path] = f.Content
	}
	assert.Equal(t, "key", paths["/run/kubernetes/pki/sa.key"])
	assert.Equal(t, "pub", paths["/run/kubernetes/pki/sa.pub"])
}

func TestKubernetesUserData_EveryRestartNamesARunningService(t *testing.T) {
	for _, controlPlane := range []bool{true, false} {
		s := kubeUserData(t, kubeNode(t, controlPlane, nil))
		running := map[string]bool{}
		for _, name := range serviceNames(s) {
			running[name] = true
		}
		for _, c := range s.Spec.PKI.Certificates {
			for _, r := range c.Restart {
				assert.True(t, running[r], "control-plane %v: %s restarts %s, which does not run", controlPlane, c.Name, r)
			}
		}
	}
}

func TestKubernetesUserData_UsesNoMvmdFieldsAndRefusesBadDescriptions(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*layout.Node)
		want   string
	}{
		"pod prefix outside the network":  {func(n *layout.Node) { n.Kube.PodPrefix = netip.MustParsePrefix("10.200.1.0/24") }, "not in the Pod network"},
		"pod prefix with host bits":       {func(n *layout.Node) { n.Kube.PodPrefix = netip.MustParsePrefix("10.64.2.1/24") }, "canonical"},
		"peer prefix overlaps the Node's": {func(n *layout.Node) { n.Kube.Peers[0].PodPrefix = netip.MustParsePrefix("10.64.2.0/25") }, "overlaps"},
		"peer off the Node's network":     {func(n *layout.Node) { n.Kube.Peers[0].Address = netip.MustParseAddr("172.16.0.1") }, "not another address"},
		"network over the Service range":  {func(n *layout.Node) { n.Kube.PodNetwork = netip.MustParsePrefix("10.0.0.0/8") }, "overlaps the Service range"},
		"disable of a stranger":           {func(n *layout.Node) { n.Disable = []string{"gaon-apiserver"} }, "cannot disable"},
		"registry CA without an image":    {func(n *layout.Node) { n.Kube.RegistryCA = "/ca.pem" }, "sandbox image"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := kubeNode(t, false, c.change).UserData()

			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}
