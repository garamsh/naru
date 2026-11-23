package layout_test

import (
	"crypto/x509"
	"encoding/pem"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/wire"
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
			BridgeNetfilter bool `yaml:"bridgeNetfilter"`
			Routes          []struct {
				To  string `yaml:"to"`
				Via string `yaml:"via"`
			} `yaml:"routes"`
		} `yaml:"network"`
		Services []struct {
			Name         string   `yaml:"name"`
			Args         []string `yaml:"args"`
			ControlPlane bool     `yaml:"controlPlane"`
			Binary       string   `yaml:"binary"`
			Restart      string   `yaml:"restartPolicy"`
		} `yaml:"services"`
		Files []struct {
			Path    string   `yaml:"path"`
			Content string   `yaml:"content"`
			Mode    string   `yaml:"mode"`
			Restart []string `yaml:"restart"`
		} `yaml:"files"`
		Storage []struct {
			Name   string `yaml:"name"`
			Serial string `yaml:"serial"`
		} `yaml:"storage"`
		Extensions []map[string]string `yaml:"extensions"`
		PKI        struct {
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
	assert.Equal(t, []string{"containerd", "addon-image-coredns", "addon-image-volumes", "kube-network-policies", "kubelet", "kube-proxy"}, running, "what a worker runs")
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

		assert.Equal(t, wire.DataLabel, s.Spec.DataDisk.Label)
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

func argsOf(s kubeSpec, service string) string {
	for _, sv := range s.Spec.Services {
		if sv.Name == service {
			return strings.Join(sv.Args, " ")
		}
	}
	return ""
}

func TestKubernetesUserData_ControlPlaneThatAcceptsTokensCarriesTheListAndRestartsTheAPIServerOnIt(t *testing.T) {
	n := kubeNode(t, true, func(n *layout.Node) {
		n.Kube.BootstrapTokens = true
		n.Kube.Tokens = []string{"abcdef.0123456789abcdef"}
	})

	s := kubeUserData(t, n)

	assert.Contains(t, argsOf(s, "kube-apiserver"), "--token-auth-file=/run/kubernetes/bootstrap-tokens.csv")
	assert.Contains(t, argsOf(s, "kube-controller-manager"), "--cluster-signing-key-file=/run/kubernetes/pki/ca.key")
	found := false
	for _, f := range s.Spec.Files {
		if f.Path == layout.KubeBootstrapTokensFile {
			found = true
			assert.Equal(t, "abcdef.0123456789abcdef,system:bootstrap:abcdef,abcdef,\"system:bootstrappers\"\n", f.Content)
			assert.Equal(t, []string{"kube-apiserver"}, f.Restart)
		}
	}
	assert.True(t, found, "the list is a file")

	plain := kubeUserData(t, kubeNode(t, true, nil))
	assert.NotContains(t, argsOf(plain, "kube-apiserver"), "--token-auth-file", "control: a Node not given tokens takes none")
}

func TestKubernetesUserData_WorkerWithATokenPresentsItInPlaceOfAnIssuedKubeletClientPair(t *testing.T) {
	n := kubeNode(t, false, func(n *layout.Node) { n.Kube.Token = "abcdef.0123456789abcdef" })

	s := kubeUserData(t, n)

	assert.Contains(t, argsOf(s, "kubelet"), "--bootstrap-kubeconfig=/run/kubernetes/bootstrap-kubelet.conf")
	paths := map[string]string{}
	for _, f := range s.Spec.Files {
		paths[f.Path] = f.Content
	}
	assert.Contains(t, paths["/run/kubernetes/bootstrap-kubelet.conf"], "token: abcdef.0123456789abcdef")
	assert.NotContains(t, paths, "/run/kubernetes/pki/kubelet-client.crt")
	assert.Contains(t, paths, "/run/kubernetes/pki/kubelet.crt", "the serving pair is still issued")
}

func TestKubernetesUserData_TokensAreRefusedOnTheWrongRole(t *testing.T) {
	for name, change := range map[string]func(*layout.Node){
		"a worker accepting tokens":       func(n *layout.Node) { n.Kube.BootstrapTokens = true },
		"a control-plane Node presenting": func(n *layout.Node) { n.Kube.Token = "abcdef.0123456789abcdef" },
	} {
		cp := name[0:3] != "a w"
		_, err := kubeNode(t, cp, change).UserData()
		assert.Error(t, err, name)
	}
}

func TestKubeTokensContent_EmptyListIsAnEmptyFileTheAPIServerAcceptsAndEveryTokenIsALine(t *testing.T) {
	// k8s.io/apiserver v0.37.1 tokenfile.NewCSV reads records until io.EOF, so an empty file is an
	// empty token map and no error (ADR 0081).
	assert.Equal(t, "", layout.KubeTokensContent(nil))
	assert.Equal(t, 2, strings.Count(layout.KubeTokensContent([]string{"abcdef.0123456789abcdef", "fedcba.fedcba9876543210"}), "\n"))
}

func TestKubernetesUserData_ControlPlaneThatAcceptsTokensWithNoneStillCarriesTheEmptyPrivateFileSoTheFirstTokenIsAPatch(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, func(n *layout.Node) { n.Kube.BootstrapTokens = true }))

	assert.Contains(t, argsOf(s, "kube-apiserver"), "--token-auth-file="+layout.KubeBootstrapTokensFile)
	var got []string
	for _, f := range s.Spec.Files {
		if f.Path == layout.KubeBootstrapTokensFile {
			got = append(got, f.Mode+"|"+f.Content)
		}
	}
	assert.Equal(t, []string{"0600|"}, got)
}

func TestKubernetesUserData_WorkerWithATokenKeepsItsKubeProxyPairAndKubeconfig(t *testing.T) {
	// kube-proxy authenticates as system:kube-proxy, which system:node-proxier is bound to in
	// k8s.io/kubernetes plugin/pkg/auth/authorizer/rbac/bootstrappolicy/policy.go, so the issued
	// pair is kept; the token replaces only the kubelet's client pair (ADR 0081).
	s := kubeUserData(t, kubeNode(t, false, func(n *layout.Node) { n.Kube.Token = "abcdef.0123456789abcdef" }))

	paths := map[string]string{}
	for _, f := range s.Spec.Files {
		paths[f.Path] = f.Content
	}
	assert.Contains(t, paths, "/run/kubernetes/pki/kube-proxy.crt")
	assert.Contains(t, paths, "/run/kubernetes/pki/kube-proxy.key")
	assert.Contains(t, paths["/run/kubernetes/kube-proxy.conf"], "client-certificate")
}

func serviceByName(t *testing.T, s kubeSpec, name string) (args []string, binary, restart string) {
	t.Helper()
	for _, sv := range s.Spec.Services {
		if sv.Name == name {
			return sv.Args, sv.Binary, sv.Restart
		}
	}
	require.Failf(t, "no such service", "%s is not in %v", name, serviceNames(s))
	return nil, "", ""
}

func fileContent(t *testing.T, s kubeSpec, path string) string {
	t.Helper()
	for _, f := range s.Spec.Files {
		if f.Path == path {
			return f.Content
		}
	}
	require.Failf(t, "no such file", "%s", path)
	return ""
}

func TestKubernetesUserData_KubeletGivesPodsTheClusterDNSAtTheAddressOfTheKubeDNSService(t *testing.T) {
	for _, controlPlane := range []bool{true, false} {
		s := kubeUserData(t, kubeNode(t, controlPlane, nil))

		var kubelet struct {
			ClusterDNS    []string `yaml:"clusterDNS"`
			ClusterDomain string   `yaml:"clusterDomain"`
		}
		require.NoError(t, yaml.Unmarshal([]byte(fileContent(t, s, layout.KubeDir+"/kubelet.yaml")), &kubelet))
		assert.Equal(t, []string{"10.96.0.10"}, kubelet.ClusterDNS, "control-plane %v", controlPlane)
		assert.Equal(t, layout.ClusterDomain, kubelet.ClusterDomain)
	}
}

func TestKubernetesUserData_ControlPlaneAppliesTheEnabledAddonsWithItsOwnPairAndRunsUntilItSucceeds(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, nil))

	args, binary, restart := serviceByName(t, s, "addons")
	assert.Equal(t, "/bin/kubectl", binary)
	assert.Equal(t, "on-failure", restart)
	assert.Contains(t, args, "--server-side")
	assert.Contains(t, args, "--field-manager=naru-addons")
	assert.Contains(t, args, layout.AddonsDir+"/manifests/dns.yaml")
	var found bool
	for _, c := range s.Spec.PKI.Certificates {
		if c.Name == "addons" {
			found = true
			assert.Equal(t, "system:masters", c.Organization)
			assert.Equal(t, "client", c.Usage)
			assert.Equal(t, []string{"addons"}, c.Restart)
		}
	}
	assert.True(t, found, "init issues the add-ons' pair")
}

func TestKubernetesUserData_DisabledDNSIsNeitherAppliedNorGivenAPairButItsImageIsStillImported(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, func(n *layout.Node) {
		n.Kube.DisabledAddons = []string{"dns", "volumes"}
		n.Kube.DisableNetworkPolicy = true
	}))

	assert.NotContains(t, serviceNames(s), "addons", "no add-on is left to apply")
	for _, c := range s.Spec.PKI.Certificates {
		assert.NotEqual(t, "addons", c.Name)
	}
	assert.Contains(t, serviceNames(s), "addon-image-coredns")
	assert.NotContains(t, serviceNames(s), "addon-image-volumes", "a disabled volume add-on's extension is large: it is not imported")
}

func TestKubernetesUserData_EveryNodeImportsTheAddonImagesUnderTheNameTheManifestsUseAndPinsThemUntilThatSucceeds(t *testing.T) {
	for _, controlPlane := range []bool{true, false} {
		s := kubeUserData(t, kubeNode(t, controlPlane, nil))

		args, binary, restart := serviceByName(t, s, "addon-image-coredns")
		assert.Equal(t, "/bin/ctr", binary)
		assert.Equal(t, "on-failure", restart)
		line := strings.Join(args, " ")
		assert.Contains(t, line, "--namespace k8s.io images import --digests", "the CRI reads the k8s.io namespace")
		assert.Contains(t, line, "--base-name naru.local/coredns ", "without it ctr names the image import-<date>, and the manifest's reference is never created")
		assert.Contains(t, line, "--label io.cri-containerd.pinned=pinned", "the tag and the digest image are pinned as they are imported")
		assert.Equal(t, layout.AddonsDir+"/images/coredns.tar", args[len(args)-1])
		assert.NotContains(t, serviceNames(s), "addon-pin-coredns")
	}
}

func TestKubernetesUserData_RefusesAnUnknownAddonToDisable(t *testing.T) {
	_, err := kubeNode(t, true, func(n *layout.Node) { n.Kube.DisabledAddons = []string{"nope"} }).UserData()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "dns")
}

func volumesFile(s kubeSpec, path string) (string, bool) {
	for _, f := range s.Spec.Files {
		if f.Path == path {
			return f.Content, true
		}
	}
	return "", false
}

func TestKubernetesUserData_VolumesAreAnUnpinnedExtensionWhoseManifestsAndImageAreItsOwn(t *testing.T) {
	cp := kubeUserData(t, kubeNode(t, true, nil))

	require.Len(t, cp.Spec.Extensions, 1)
	assert.Equal(t, "naru-volumes", cp.Spec.Extensions[0]["name"])
	assert.Empty(t, cp.Spec.Extensions[0]["sha256"], "an entry with no sha256 is resolved by init to the image built for its version (ADR 0084)")
	dir := "/usr/lib/naru/extensions/naru-volumes/addons"
	args, _, _ := serviceByName(t, cp, "addons")
	assert.Contains(t, args, dir+"/manifests/volumes.yaml")
	assert.Contains(t, args, layout.AddonsDir+"/manifests/dns.yaml")
	imp, binary, restart := serviceByName(t, cp, "addon-image-volumes")
	assert.Equal(t, "/bin/ctr", binary)
	assert.Equal(t, "on-failure", restart)
	assert.Equal(t, dir+"/images/volumes.tar", imp[len(imp)-1])
	assert.Contains(t, strings.Join(imp, " "), "--base-name naru.local/volumes ")
	names := serviceNames(cp)
	assert.Less(t, slicesIndex(names, "containerd"), slicesIndex(names, "addon-image-volumes"))
}

func slicesIndex(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func TestKubernetesUserData_DisablingVolumesLeavesNoExtensionNoConfigAndNoLabel(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, func(n *layout.Node) {
		n.Kube.DisabledAddons = []string{"volumes"}
		n.Storage = []layout.StorageDisk{{Name: "data", Serial: "s1"}}
	}))

	assert.Empty(t, s.Spec.Extensions)
	assert.NotContains(t, serviceNames(s), "addon-image-volumes")
	_, found := volumesFile(s, layout.VolumesConfig)
	assert.False(t, found)
	args, _, _ := serviceByName(t, s, "kubelet")
	assert.NotContains(t, strings.Join(args, " "), layout.VolumesNodeLabel)
	args, _, _ = serviceByName(t, s, "addons")
	assert.NotContains(t, strings.Join(args, " "), "volumes.yaml")
}

func TestKubernetesUserData_ANodeWithDisksGetsTheDriversConfigAndTheLabelThatSelectsItsPod(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, false, func(n *layout.Node) {
		n.Storage = []layout.StorageDisk{{Name: "data", Serial: "s1"}, {Name: "more", Serial: "s2"}}
		n.Kube.BlockSerials = []string{"b1", "b2"}
	}))

	cfg, found := volumesFile(s, layout.VolumesConfig)
	require.True(t, found)
	assert.JSONEq(t, `{"stateDir":"/var/lib/naru/volumes","storageDir":"/var/lib/naru/storage/data","blockSerials":["b1","b2"]}`, cfg, "local volumes are on the first storage disk")
	assert.Contains(t, cfg, config.StorageRoot+"/data", "where init mounts the first storage disk")
	require.Len(t, s.Spec.Storage, 2, "init formats and mounts the storage disks")
	assert.Equal(t, "s1", s.Spec.Storage[0].Serial)
	args, _, _ := serviceByName(t, s, "kubelet")
	assert.Contains(t, args, "--node-labels=naru.garam.sh/volumes=true")
}

func TestKubernetesUserData_ABlockOnlyNodeOffersNoLocalVolumes(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, false, func(n *layout.Node) { n.Kube.BlockSerials = []string{"b1"} }))

	cfg, _ := volumesFile(s, layout.VolumesConfig)
	assert.JSONEq(t, `{"stateDir":"/var/lib/naru/volumes","blockSerials":["b1"]}`, cfg)
}

func TestKubernetesUserData_ANodeWithNoDiskIsNotLabelledAndHasNoConfig(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, false, nil))

	_, found := volumesFile(s, layout.VolumesConfig)
	assert.False(t, found)
	args, _, _ := serviceByName(t, s, "kubelet")
	assert.NotContains(t, strings.Join(args, " "), layout.VolumesNodeLabel)
}

func TestKubernetesUserData_LabelAndBootstrapFlagsBothReachTheKubelet(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, false, func(n *layout.Node) {
		n.Kube.BlockSerials = []string{"b1"}
		n.Kube.Token = "abcdef.0123456789abcdef"
	}))

	args, _, _ := serviceByName(t, s, "kubelet")
	assert.Contains(t, args, "--node-labels=naru.garam.sh/volumes=true")
	assert.Contains(t, strings.Join(args, " "), "--bootstrap-kubeconfig=")
}

func TestKubernetesUserData_RefusesBlockSerialsInitWouldFormat(t *testing.T) {
	for name, serials := range map[string][]string{"empty": {""}, "repeated": {"b", "b"}, "a storage disk's": {"s1"}} {
		_, err := kubeNode(t, false, func(n *layout.Node) {
			n.Storage = []layout.StorageDisk{{Name: "data", Serial: "s1"}}
			n.Kube.BlockSerials = serials
		}).UserData()
		assert.Error(t, err, name)
	}
}

func TestKubernetesUserData_RefusesAStorageDiskInitWouldRefuse(t *testing.T) {
	_, err := kubeNode(t, false, func(n *layout.Node) { n.Storage = []layout.StorageDisk{{Name: "Bad_Name", Serial: "s"}} }).UserData()

	require.Error(t, err)
}

// issuerFlags are the API server's --service-account-issuer values, in order, and its JWKS URI flag.
func issuerFlags(t *testing.T, n layout.Node) (issuers []string, jwksURI string) {
	t.Helper()
	args, _, _ := serviceByName(t, kubeUserData(t, n), "kube-apiserver")
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--service-account-issuer="); ok {
			issuers = append(issuers, v)
		}
		if v, ok := strings.CutPrefix(a, "--service-account-jwks-uri="); ok {
			jwksURI = v
		}
	}
	return issuers, jwksURI
}

func TestKubernetesUserData_NoIssuerSetKeepsTheInClusterIssuerAloneAndTheDiscoveryDocumentsDefault(t *testing.T) {
	issuers, jwksURI := issuerFlags(t, kubeNode(t, true, nil))

	assert.Equal(t, []string{"https://kubernetes.default.svc.cluster.local"}, issuers)
	assert.Empty(t, jwksURI)
}

func TestKubernetesUserData_TheOperatorsIssuersComeFirstInOrderAndTheInClusterIssuerIsStillAccepted(t *testing.T) {
	n := kubeNode(t, true, func(n *layout.Node) {
		n.Kube.ServiceAccountIssuers = []string{"https://oidc.example.com/naru", "https://other.example.com"}
	})

	issuers, jwksURI := issuerFlags(t, n)

	assert.Equal(t, []string{"https://oidc.example.com/naru", "https://other.example.com", "https://kubernetes.default.svc.cluster.local"}, issuers,
		"the first signs, and the in-cluster issuer is accepted last")
	assert.Equal(t, "https://oidc.example.com/naru/openid/v1/jwks", jwksURI, "the first issuer's key set")
}

func TestKubernetesUserData_AnIssuerThatNamesTheInClusterOneIsNotRepeated(t *testing.T) {
	n := kubeNode(t, true, func(n *layout.Node) {
		n.Kube.ServiceAccountIssuers = []string{"https://kubernetes.default.svc.cluster.local"}
	})

	issuers, _ := issuerFlags(t, n)

	assert.Equal(t, []string{"https://kubernetes.default.svc.cluster.local"}, issuers)
}

func TestKubernetesUserData_RefusesAnIssuerOpenIDConnectDoesNotAllow(t *testing.T) {
	for _, bad := range []string{"http://oidc.example.com", "https://oidc.example.com?a=b", "https://oidc.example.com#f", "https://oidc.example.com/", "oidc.example.com", "https://",
		"https://oidc.example.com/..", "https://oidc.example.com/a/../b", "https://oidc.example.com/./a", "https://oidc.example.com//a",
		"https://oidc.example.com/%2e%2e/a", "https://oidc.example.com/%2E/a", "https://u:p@oidc.example.com", "https://@oidc.example.com",
		"https://oidc.example.com/a b", "https://oidc.example.com/a\nb", "https://oidc.example.com /a", "HTTPS://oidc.example.com", "Https://oidc.example.com",
		"https://oidc.example.com/a\\b", "https://oidc.example.com\\@evil.example.com"} {
		n := kubeNode(t, true, func(n *layout.Node) { n.Kube.ServiceAccountIssuers = []string{bad} })
		_, err := n.UserData()
		assert.ErrorContains(t, err, "ServiceAccount issuer", bad)
	}
	n := kubeNode(t, true, func(n *layout.Node) {
		n.Kube.ServiceAccountIssuers = []string{"https://a.example.com", "https://a.example.com"}
	})
	_, err := n.UserData()
	assert.ErrorContains(t, err, "ServiceAccount issuer", "repeated")
	ok := kubeNode(t, true, func(n *layout.Node) { n.Kube.ServiceAccountIssuers = []string{"https://a.example.com/path"} })
	_, err = ok.UserData()
	assert.NoError(t, err, "control: a valid issuer with a path")
	for _, good := range []string{"https://oidc.example.com", "https://oidc.example.com:8443/v1.2/a_b-c~d", "https://OIDC.example.com/Tenant"} {
		n := kubeNode(t, true, func(n *layout.Node) { n.Kube.ServiceAccountIssuers = []string{good} })
		_, err := n.UserData()
		assert.NoError(t, err, "control: "+good)
	}
}

func TestKubernetesUserData_AnAppliedKeyChangeRestartsTheServicesThatReadTheKeys(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, nil))

	restarts := map[string][]string{}
	for _, f := range s.Spec.Files {
		restarts[f.Path] = f.Restart
	}
	assert.Equal(t, []string{"kube-apiserver", "kube-controller-manager"}, restarts["/run/kubernetes/pki/sa.key"])
	assert.Equal(t, []string{"kube-apiserver"}, restarts["/run/kubernetes/pki/sa.pub"])
}

// certificate is the pair of spec.pki that the test finds by name.
func certificate(t *testing.T, s kubeSpec, name string) (cn, org string, found bool) {
	t.Helper()
	for _, c := range s.Spec.PKI.Certificates {
		if c.Name == name {
			return c.CommonName, c.Organization, true
		}
	}
	return "", "", false
}

func TestKubernetesUserData_NetworkPolicyEnforcerRunsBeforeTheKubeletWithItsOwnPairAndKubeconfigAndTheNodeBridgesThroughNetfilter(t *testing.T) {
	for _, controlPlane := range []bool{true, false} {
		s := kubeUserData(t, kubeNode(t, controlPlane, nil))

		names := serviceNames(s)
		require.Contains(t, names, "kube-network-policies", "control-plane %v", controlPlane)
		assert.Less(t, slices.Index(names, "kube-network-policies"), slices.Index(names, "kubelet"), "no Pod starts unenforced")
		args, binary, _ := serviceByName(t, s, "kube-network-policies")
		assert.Equal(t, "/bin/kube-network-policies", binary)
		assert.Contains(t, args, "--kubeconfig="+layout.KubeDir+"/kube-network-policies.conf")
		assert.Contains(t, args, "--disable-nri", "NRI is off in containerd, and the agent takes Pod IPs from the API server")
		assert.Contains(t, args, "--fail-open=false", "upstream defaults to true: the agent fails closed only when told to (ADR 0086 item 3)")
		assert.Contains(t, args, "--metrics-bind-address=127.0.0.1:9080", "upstream defaults to :9080 on every address, and a Node listens on no port its configuration does not name (ADR 0047 item 4)")
		assert.Contains(t, fileContent(t, s, layout.KubeDir+"/kube-network-policies.conf"), "client-certificate: "+layout.KubePKIDir+"/kube-network-policies.crt")
		assert.True(t, s.Spec.Network.BridgeNetfilter, "the bridge passes Pod traffic to the netfilter hooks the agent's rules sit on")
	}
}

func TestKubernetesUserData_NetworkPolicyEnforcerIsOutOfAControlPlaneNodesPairsFilesAndServicesWhenTheClusterTurnsItOff(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, func(n *layout.Node) { n.Kube.DisableNetworkPolicy = true }))

	assert.NotContains(t, serviceNames(s), "kube-network-policies")
	_, _, found := certificate(t, s, "kube-network-policies")
	assert.False(t, found, "no pair is issued for an agent that does not run")
	for _, f := range s.Spec.Files {
		assert.NotContains(t, f.Path, "kube-network-policies")
		assert.NotEqual(t, layout.NetworkPolicyRBACFile, f.Path)
	}
	assert.False(t, s.Spec.Network.BridgeNetfilter)
	args, _, _ := serviceByName(t, s, "addons")
	assert.NotContains(t, args, layout.NetworkPolicyRBACFile)
}

func TestKubernetesUserData_WorkerCarriesTheEnforcersPairAsFilesOnlyWhileTheClusterRunsIt(t *testing.T) {
	on := kubeUserData(t, kubeNode(t, false, nil))
	off := kubeUserData(t, kubeNode(t, false, func(n *layout.Node) { n.Kube.DisableNetworkPolicy = true }))

	for _, ext := range []string{".crt", ".key"} {
		assert.NotEmpty(t, fileContent(t, on, layout.KubePKIDir+"/kube-network-policies"+ext))
	}
	for _, f := range off.Spec.Files {
		assert.NotContains(t, f.Path, "kube-network-policies", "a worker of a cluster that turned it off carries nothing of it")
	}
	assert.NotContains(t, serviceNames(off), "kube-network-policies")
	assert.False(t, off.Spec.Network.BridgeNetfilter)
}

func TestKubernetesUserData_NetworkPolicyEnforcerIsAUserOfItsOwnWithNoGroup(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, nil))

	cn, org, found := certificate(t, s, "kube-network-policies")
	require.True(t, found)
	assert.Equal(t, layout.NetworkPolicyUser, cn)
	assert.Empty(t, org, "system:nodes would name a group the Node authorizer does not decide for and nothing binds")

	n := kubeNode(t, false, nil)
	files := map[string]string{}
	for _, f := range kubeUserData(t, n).Spec.Files {
		files[f.Path] = f.Content
	}
	block, _ := pem.Decode([]byte(files[layout.KubePKIDir+"/kube-network-policies.crt"]))
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, layout.NetworkPolicyUser, cert.Subject.CommonName)
	assert.Empty(t, cert.Subject.Organization)
}

func TestKubernetesUserData_ControlPlaneAppliesTheEnforcersRBACWhateverAddonsAreDisabled(t *testing.T) {
	s := kubeUserData(t, kubeNode(t, true, func(n *layout.Node) { n.Kube.DisabledAddons = []string{"dns", "volumes"} }))

	args, _, restart := serviceByName(t, s, "addons")
	assert.Equal(t, "on-failure", restart)
	assert.Contains(t, args, layout.NetworkPolicyRBACFile)
	assert.NotContains(t, args, layout.AddonsDir+"/manifests/dns.yaml", "the disabled add-on stays out")
	assert.Equal(t, layout.NetworkPolicyRBAC, fileContent(t, s, layout.NetworkPolicyRBACFile))
	_, _, found := certificate(t, s, "addons")
	assert.True(t, found, "the service applies as the add-ons' pair")
	assert.NotEmpty(t, fileContent(t, s, layout.KubeDir+"/addons.conf"))

	// The control: a Worker applies nothing, so it carries no manifest.
	for _, f := range kubeUserData(t, kubeNode(t, false, nil)).Spec.Files {
		assert.NotEqual(t, layout.NetworkPolicyRBACFile, f.Path)
	}
}

func TestNetworkPolicyRBAC_GrantsExactlyTheThreeListsAndWatchesTheEnforcerReadsToTheUserItsPairNames(t *testing.T) {
	type object struct {
		Kind  string `yaml:"kind"`
		Rules []struct {
			APIGroups []string `yaml:"apiGroups"`
			Resources []string `yaml:"resources"`
			Verbs     []string `yaml:"verbs"`
		} `yaml:"rules"`
		Subjects []struct{ Kind, Name string } `yaml:"subjects"`
		RoleRef  struct{ Kind, Name string }   `yaml:"roleRef"`
		Metadata struct{ Name string }         `yaml:"metadata"`
	}
	var roles, bindings []object
	dec := yaml.NewDecoder(strings.NewReader(layout.NetworkPolicyRBAC))
	for {
		var o object
		if err := dec.Decode(&o); err != nil {
			break
		}
		switch o.Kind {
		case "ClusterRole":
			roles = append(roles, o)
		case "ClusterRoleBinding":
			bindings = append(bindings, o)
		}
	}
	require.Len(t, roles, 1)
	require.Len(t, bindings, 1)

	var granted []string
	for _, r := range roles[0].Rules {
		assert.Equal(t, []string{"list", "watch"}, r.Verbs, "the least: no get, no write")
		for _, g := range r.APIGroups {
			for _, res := range r.Resources {
				granted = append(granted, g+"/"+res)
			}
		}
	}
	assert.ElementsMatch(t, []string{"/pods", "/namespaces", "networking.k8s.io/networkpolicies"}, granted)
	assert.Equal(t, []struct{ Kind, Name string }{{"User", layout.NetworkPolicyUser}}, bindings[0].Subjects, "the binding names the CN the agent's pair is issued with")
	assert.Equal(t, roles[0].Metadata.Name, bindings[0].RoleRef.Name)
	assert.Equal(t, "ClusterRole", bindings[0].RoleRef.Kind)
}
