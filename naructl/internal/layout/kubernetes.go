package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/pki"
)

// Where a Kubernetes Node's files are, and the ports its programs serve (ADR 0031). The API server
// serves on APIPort, the port gaon's does.
const (
	// KubeDir holds the files init writes for the Kubernetes programs, and KubePKIDir the certificates.
	KubeDir    = "/run/kubernetes"
	KubePKIDir = KubeDir + "/pki"
	// ContainerdSocket is containerd's CRI endpoint, ContainerdRoot its image and snapshot store, and
	// KubeletRoot the kubelet's, both on the data disk.
	ContainerdSocket = "/run/containerd/containerd.sock"
	ContainerdRoot   = DataMount + "/containerd"
	KubeletRoot      = DataMount + "/kubelet"
	// CNIConfDir is where containerd reads the network configuration, and CNIBinDir the plugins the image carries.
	CNIConfDir = "/run/cni/net.d"
	CNIBinDir  = "/opt/cni/bin"
	// CertsDir is where containerd reads the trust of a registry.
	CertsDir = "/run/containerd/certs.d"
	// DelegatedCgroup is the cgroup init keeps for the kubelet's pod cgroups (config.DelegatedName).
	DelegatedCgroup = "/naru/kubernetes"

	KubeletPort           = "10250"
	ControllerManagerPort = "10257"
	SchedulerPort         = "10259"
	ProxyPort             = "10256"

	// ServiceCIDR is the range of Service cluster addresses; the API server's own Service is its first address.
	ServiceCIDR = "10.96.0.0/16"
	// ClusterDomain is the domain of the cluster's DNS names, which the API server's certificate names.
	ClusterDomain = "cluster.local"
)

// Kubernetes is the Topology of a Kubernetes Node: the services in the order init starts them.
var Kubernetes = Topology{
	Services: []Service{
		{Name: "registry"},
		{Name: "containerd"},
		{Name: "etcd", ControlPlane: true},
		{Name: "kube-apiserver", ControlPlane: true},
		{Name: "kube-controller-manager", ControlPlane: true},
		{Name: "kube-scheduler", ControlPlane: true},
		{Name: "kubelet"},
		{Name: "kube-proxy"},
	},
	APIServer: "kube-apiserver",
	Agent:     "kubelet",
}

// KubePeer is another Node of the cluster: its address, which is the next hop to its Pods, and the
// IPv4 prefix its Pods take addresses from.
type KubePeer struct {
	Address   netip.Addr
	PodPrefix netip.Prefix
}

// KubeNode says what makes a Node a Kubernetes Node (ADR 0031). Of Node's fields, Name, Address and
// the network-config fields, APIServer (the address the API server is dialled at), CA (the cluster's
// authority: only a control-plane Node carries its key), TimeServers, Registries, Binaries and
// Disable apply; the Machine, token and Mode fields do not.
type KubeNode struct {
	// ControlPlane makes the Node run etcd and the control-plane programs besides the kubelet.
	ControlPlane bool
	// PodNetwork is the prefix every Pod address is in, and PodPrefix the part of it this Node's Pods take.
	PodNetwork, PodPrefix netip.Prefix
	// Peers are the other Nodes: the Node routes each one's PodPrefix via its Address.
	Peers []KubePeer
	// FrontProxy is the aggregation layer's authority, and ServiceAccountKey and ServiceAccountPub
	// the PEM of the keys of ServiceAccount tokens: all three on a control-plane Node only.
	FrontProxy                           *pki.CA
	ServiceAccountKey, ServiceAccountPub []byte
	// LocalRegistry makes the Node run the registry service from the data disk's inputs, and
	// RegistryCA is the path on the Node of the CA containerd trusts for RegistryAddr.
	LocalRegistry bool
	RegistryCA    string
	// SandboxImage is the image of a Pod's sandbox container.
	SandboxImage string
}

var (
	apiserverKubelet = pairSpec{"apiserver-kubelet-client", "kube-apiserver-kubelet-client"}
	frontProxy       = pairSpec{"front-proxy-client", "front-proxy-client"}
)

func kubePath(name string) string { return path.Join(KubeDir, name) }

func pkiPath(name string) string { return path.Join(KubePKIDir, name) }

func (n Node) kube() KubeNode { return *n.Kube }

// checkKube reports the first thing wrong with a Kubernetes Node's description.
func (n Node) checkKube() error {
	k := n.kube()
	switch {
	case n.Name == "":
		return errors.New("a Node needs a name")
	case !dnsLabel.MatchString(n.Name):
		return fmt.Errorf("the name %q is not a DNS label; it is the Node's name in the cluster", n.Name)
	case !n.Address.IsValid() || !n.Address.Addr().Is4():
		return errors.New("the address is not an IPv4 address with a prefix length")
	case !n.APIServer.IsValid() || !n.APIServer.Is4():
		return errors.New("the API server's address is not an IPv4 address")
	case n.CA == nil:
		return errors.New("the cluster's authority is not set")
	case !k.PodNetwork.IsValid() || !k.PodNetwork.Addr().Is4() || k.PodNetwork != k.PodNetwork.Masked():
		return errors.New("the Pod network is not an IPv4 prefix in its canonical form")
	case !k.PodPrefix.IsValid() || !k.PodPrefix.Addr().Is4() || k.PodPrefix != k.PodPrefix.Masked():
		return errors.New("the Pod prefix is not an IPv4 prefix in its canonical form")
	case !k.PodNetwork.Contains(k.PodPrefix.Addr()) || k.PodPrefix.Bits() < k.PodNetwork.Bits():
		return fmt.Errorf("the Pod prefix %s is not in the Pod network %s", k.PodPrefix, k.PodNetwork)
	case k.PodNetwork.Overlaps(netip.MustParsePrefix(ServiceCIDR)) || n.Address.Masked().Overlaps(k.PodNetwork):
		return fmt.Errorf("the Pod network %s overlaps the Service range %s or the Node's network %s", k.PodNetwork, ServiceCIDR, n.Address.Masked())
	case k.ControlPlane && (k.FrontProxy == nil || len(k.ServiceAccountKey) == 0 || len(k.ServiceAccountPub) == 0):
		return errors.New("a control-plane Node needs the front-proxy authority and the ServiceAccount keys")
	case k.RegistryCA != "" && !path.IsAbs(k.RegistryCA):
		return fmt.Errorf("the registry CA path %q is not absolute", k.RegistryCA)
	case k.RegistryCA != "" && k.SandboxImage == "":
		return errors.New("a registry CA is for the sandbox image, which is not set")
	}
	for i, p := range k.Peers {
		switch {
		case !p.Address.IsValid() || !p.Address.Is4() || !n.Address.Masked().Contains(p.Address) || p.Address == n.Address.Addr():
			return fmt.Errorf("peer %d: the address %s is not another address on the Node's network %s", i+1, p.Address, n.Address.Masked())
		case !p.PodPrefix.IsValid() || p.PodPrefix != p.PodPrefix.Masked() || !k.PodNetwork.Contains(p.PodPrefix.Addr()) || p.PodPrefix.Bits() < k.PodNetwork.Bits():
			return fmt.Errorf("peer %s: the Pod prefix %s is not in the Pod network %s", p.Address, p.PodPrefix, k.PodNetwork)
		case p.PodPrefix.Overlaps(k.PodPrefix):
			return fmt.Errorf("peer %s: the Pod prefix %s overlaps the Node's own %s", p.Address, p.PodPrefix, k.PodPrefix)
		}
		for _, q := range k.Peers[:i] {
			if q.Address == p.Address || q.PodPrefix.Overlaps(p.PodPrefix) {
				return fmt.Errorf("peers %s and %s repeat an address or overlap in Pod prefixes", q.Address, p.Address)
			}
		}
	}
	for _, name := range n.Disable {
		if !Kubernetes.Has(name) {
			return fmt.Errorf("cannot disable %q: the services that can be left out are %s", name, strings.Join(Kubernetes.Names(), ", "))
		}
	}
	for name, bin := range n.Binaries {
		if !Kubernetes.Has(name) || !path.IsAbs(bin) {
			return fmt.Errorf("cannot give %q the binary %q: a service of %s, and an absolute path", name, bin, strings.Join(Kubernetes.Names(), ", "))
		}
	}
	return nil
}

// kubeUserData is the user-data of a Kubernetes Node.
func (n Node) kubeUserData() ([]byte, error) {
	if err := n.checkKube(); err != nil {
		return nil, err
	}
	k := n.kube()
	var c nodeConfig
	c.APIVersion = "naru.garam.sh/v1alpha1"
	c.Kind = "NodeConfig"
	c.Metadata.Name = "naru-" + n.Name
	c.Spec.ControlPlane = k.ControlPlane
	c.Spec.DataDisk.Label = DataLabel
	c.Spec.DataDisk.MountPoint = DataMount
	// A Kubernetes Node may be a microVM, whose host gives it a data disk it cannot format; a disk
	// already labelled is left as it is (docs/architecture/init.md).
	c.Spec.DataDisk.FormatBlank = true
	c.Spec.Network.Forwarding = []string{"ipv4"}
	for _, p := range k.Peers {
		c.Spec.Network.Routes = append(c.Spec.Network.Routes, route{To: p.PodPrefix, Via: p.Address})
	}
	files, err := n.kubeFiles()
	if err != nil {
		return nil, err
	}
	c.Spec.Files = files
	c.Spec.PKI = n.kubePKI()
	if len(n.TimeServers) > 0 {
		c.Spec.Time = &timeSpec{Servers: n.TimeServers}
	}
	c.Spec.Registries = n.registryEntries()
	c.Spec.Services = n.kubeServices()
	n.withoutDisabled(&c)
	return yaml.Marshal(c)
}

func (n Node) kubeEnv() map[string]string { return map[string]string{"PATH": "/bin"} }

// kubeServices are the Node's, in the order init starts them: the registry, containerd, etcd, then
// the programs that dial what is before them, the control plane before the kubelet and kube-proxy.
func (n Node) kubeServices() []service {
	k := n.kube()
	ca := pkiPath("ca.crt")
	list := []service{
		{Name: "registry", Binary: n.binary("registry", path.Join(InputsDir, "bin/naru-registry")), Args: []string{"-listen", RegistryAddr, "-dir", path.Join(InputsDir, "registry")}},
		{Name: "containerd", Binary: n.binary("containerd", "/bin/containerd"), Args: []string{"--config", kubePath("containerd.toml")}, Env: n.kubeEnv()},
		{Name: "etcd", Binary: n.binary("etcd", "/bin/etcd"), Args: n.etcdArgs()},
		{
			Name: "kube-apiserver", Binary: n.binary("kube-apiserver", "/bin/kube-apiserver"),
			Args: []string{
				"--advertise-address=" + n.APIServer.String(), "--secure-port=" + APIPort,
				"--etcd-servers=" + n.etcdEndpoints(), "--etcd-cafile=" + gaonPath("ca.pem"),
				"--etcd-certfile=" + gaonPath(apiserverTo.name+".pem"), "--etcd-keyfile=" + gaonPath(apiserverTo.name+".key"),
				"--client-ca-file=" + ca, "--tls-cert-file=" + pkiPath("apiserver.crt"), "--tls-private-key-file=" + pkiPath("apiserver.key"),
				"--kubelet-certificate-authority=" + ca, "--kubelet-client-certificate=" + pkiPath(apiserverKubelet.name+".crt"),
				"--kubelet-client-key=" + pkiPath(apiserverKubelet.name+".key"), "--kubelet-preferred-address-types=InternalIP",
				"--service-cluster-ip-range=" + ServiceCIDR,
				"--service-account-issuer=https://kubernetes.default.svc." + ClusterDomain,
				"--service-account-key-file=" + pkiPath("sa.pub"), "--service-account-signing-key-file=" + pkiPath("sa.key"),
				"--authorization-mode=Node,RBAC", "--enable-admission-plugins=NodeRestriction",
				"--requestheader-client-ca-file=" + pkiPath("front-proxy-ca.crt"), "--requestheader-allowed-names=" + frontProxy.cn,
				"--requestheader-extra-headers-prefix=X-Remote-Extra-", "--requestheader-group-headers=X-Remote-Group", "--requestheader-username-headers=X-Remote-User",
				"--proxy-client-cert-file=" + pkiPath(frontProxy.name+".crt"), "--proxy-client-key-file=" + pkiPath(frontProxy.name+".key"),
			},
		},
		{
			Name: "kube-controller-manager", Binary: n.binary("kube-controller-manager", "/bin/kube-controller-manager"),
			Args: []string{
				"--kubeconfig=" + kubePath("controller-manager.conf"), "--authentication-kubeconfig=" + kubePath("controller-manager.conf"),
				"--authorization-kubeconfig=" + kubePath("controller-manager.conf"), "--bind-address=127.0.0.1", "--secure-port=" + ControllerManagerPort,
				"--cert-dir=" + kubePath("controller-manager"), "--root-ca-file=" + ca, "--use-service-account-credentials=true",
				"--service-account-private-key-file=" + pkiPath("sa.key"),
			},
		},
		{
			Name: "kube-scheduler", Binary: n.binary("kube-scheduler", "/bin/kube-scheduler"),
			Args: []string{
				"--kubeconfig=" + kubePath("scheduler.conf"), "--authentication-kubeconfig=" + kubePath("scheduler.conf"),
				"--authorization-kubeconfig=" + kubePath("scheduler.conf"), "--bind-address=127.0.0.1", "--secure-port=" + SchedulerPort,
				"--cert-dir=" + kubePath("scheduler"),
			},
		},
		{
			Name: "kubelet", Binary: n.binary("kubelet", "/bin/kubelet"), Env: n.kubeEnv(),
			Args: []string{
				"--config=" + kubePath("kubelet.yaml"), "--kubeconfig=" + kubePath("kubelet.conf"), "--hostname-override=" + n.Name,
				"--node-ip=" + n.Address.Addr().String(), "--root-dir=" + KubeletRoot,
			},
		},
		{
			Name: "kube-proxy", Binary: n.binary("kube-proxy", "/bin/kube-proxy"), Env: n.kubeEnv(),
			Args: []string{"--config=" + kubePath("kube-proxy.yaml"), "--hostname-override=" + n.Name},
		},
	}
	if !k.LocalRegistry {
		list = slices.DeleteFunc(list, func(s service) bool { return s.Name == "registry" })
	}
	for i := range list {
		list[i].ControlPlane = Kubernetes.OnlyControlPlane(list[i].Name)
	}
	return list
}

// kubePKI is a control-plane Node's spec.pki: the cluster's authority, whose key lets init issue each
// pair at boot, and the pairs of the programs the Node runs, each restarting its program when init
// renews it. A worker has none: it never carries the authority's key, and its pairs are files
// (workerPairFiles).
func (n Node) kubePKI() *pkiSpec {
	k := n.kube()
	if !k.ControlPlane {
		return nil
	}
	cert := func(spec pairSpec, authority, dir, usage, service string) pkiCertificate {
		return pkiCertificate{
			Name: spec.name, Authority: authority, CertificatePath: path.Join(dir, spec.name+certExt(dir)), KeyPath: path.Join(dir, spec.name+".key"),
			CommonName: spec.cn, Usage: usage, Restart: []string{service},
		}
	}
	cluster := func(spec pairSpec, usage, service string) pkiCertificate {
		return cert(spec, "cluster", KubePKIDir, usage, service)
	}
	spec := &pkiSpec{Authorities: []pkiAuthority{{
		Name: "cluster", Certificate: string(n.CA.CertPEM), Key: string(n.CA.KeyPEM), CertificatePath: pkiPath("ca.crt"),
	}}}
	add := func(c ...pkiCertificate) { spec.Certificates = append(spec.Certificates, c...) }
	spec.Authorities = append(spec.Authorities,
		pkiAuthority{Name: "etcd", Certificate: string(n.CA.CertPEM), Key: string(n.CA.KeyPEM), CertificatePath: gaonPath("ca.pem")},
		pkiAuthority{Name: "front-proxy", Certificate: string(k.FrontProxy.CertPEM), Key: string(k.FrontProxy.KeyPEM), CertificatePath: pkiPath("front-proxy-ca.crt")},
	)
	apiserver := cluster(pairSpec{"apiserver", "kube-apiserver"}, "server", "kube-apiserver")
	apiserver.NodeAddresses = true
	apiserver.IPAddresses = []string{n.APIServer.String(), firstHost(ServiceCIDR)}
	apiserver.DNSNames = []string{"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc." + ClusterDomain}
	kubeletClient := cluster(apiserverKubelet, "client", "kube-apiserver")
	kubeletClient.Organization = "system:masters"
	fp := cert(frontProxy, "front-proxy", KubePKIDir, "client", "kube-apiserver")
	etcdSrv := cert(etcdServer, "etcd", GaonCertDir, "server", "etcd")
	etcdSrv.NodeAddresses = true
	etcdPeerCert := cert(etcdPeer, "etcd", GaonCertDir, "client", "etcd")
	etcdPeerCert.NodeAddresses = true
	add(apiserver, kubeletClient, fp, etcdSrv, etcdPeerCert,
		cert(apiserverTo, "etcd", GaonCertDir, "client", "kube-apiserver"),
		cluster(pairSpec{"controller-manager", "system:kube-controller-manager"}, "client", "kube-controller-manager"),
		cluster(pairSpec{"scheduler", "system:kube-scheduler"}, "client", "kube-scheduler"),
	)
	node := cluster(pairSpec{"kubelet-client", "system:node:" + n.Name}, "client", "kubelet")
	node.Organization = "system:nodes"
	serving := cluster(pairSpec{"kubelet", n.Name}, "server", "kubelet")
	serving.NodeAddresses = true
	serving.DNSNames = []string{n.Name}
	add(node, serving, cluster(pairSpec{"kube-proxy", "system:kube-proxy"}, "client", "kube-proxy"))
	return spec
}

// certExt is the extension of a pair's certificate: Kubernetes names its own .crt,
// and etcd's pairs, in GaonCertDir, are .pem as gaon's are.
func certExt(dir string) string {
	if dir == GaonCertDir {
		return ".pem"
	}
	return ".crt"
}

// firstHost is the first address of the prefix, the Service the API server is behind.
func firstHost(prefix string) string { return netip.MustParsePrefix(prefix).Addr().Next().String() }

// kubeconfig is a kubeconfig that dials the API server, trusts the cluster's authority and presents
// the pair, all by path.
func (n Node) kubeconfig(user, pair string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: naru
  cluster:
    server: https://%s
    certificate-authority: %s
users:
- name: %s
  user:
    client-certificate: %s
    client-key: %s
contexts:
- name: default
  context:
    cluster: naru
    user: %s
current-context: default
`, net.JoinHostPort(n.APIServer.String(), APIPort), pkiPath("ca.crt"), user, pkiPath(pair+".crt"), pkiPath(pair+".key"), user)
}

// kubeFiles are what init writes for the Kubernetes programs: their configuration, on a worker its
// pairs, and on a control-plane Node the ServiceAccount keys, which are not issued at boot.
func (n Node) kubeFiles() ([]file, error) {
	k := n.kube()
	cni, err := json.MarshalIndent(map[string]any{
		"cniVersion": "1.0.0", "name": "naru",
		"plugins": []any{map[string]any{
			"type": "bridge", "bridge": "cni0", "isGateway": true, "isDefaultGateway": true, "ipMasq": false, "hairpinMode": true,
			"ipam": map[string]any{"type": "host-local", "ranges": [][]map[string]string{{{"subnet": k.PodPrefix.String()}}}},
		}},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	kubelet, err := yaml.Marshal(map[string]any{
		"apiVersion": "kubelet.config.k8s.io/v1beta1", "kind": "KubeletConfiguration",
		"address": n.Address.Addr().String(), "port": 10250,
		"tlsCertFile": pkiPath("kubelet.crt"), "tlsPrivateKeyFile": pkiPath("kubelet.key"),
		"authentication": map[string]any{
			"anonymous": map[string]any{"enabled": false}, "webhook": map[string]any{"enabled": true},
			"x509": map[string]any{"clientCAFile": pkiPath("ca.crt")},
		},
		"authorization":            map[string]any{"mode": "Webhook"},
		"cgroupDriver":             "cgroupfs",
		"cgroupRoot":               DelegatedCgroup,
		"containerRuntimeEndpoint": "unix://" + ContainerdSocket,
		"failSwapOn":               false,
		"podLogsDir":               KubeletRoot + "/pod-logs",
		"volumePluginDir":          KubeletRoot + "/volume-plugins",
	})
	if err != nil {
		return nil, err
	}
	proxy, err := yaml.Marshal(map[string]any{
		"apiVersion": "kubeproxy.config.k8s.io/v1alpha1", "kind": "KubeProxyConfiguration", "mode": "nftables",
		"clientConnection":   map[string]any{"kubeconfig": kubePath("kube-proxy.conf")},
		"clusterCIDR":        k.PodNetwork.String(),
		"bindAddress":        n.Address.Addr().String(),
		"healthzBindAddress": net.JoinHostPort(n.Address.Addr().String(), ProxyPort),
		"metricsBindAddress": "127.0.0.1:10249",
	})
	if err != nil {
		return nil, err
	}
	files := []file{
		{Path: kubePath("containerd.toml"), Mode: "0644", Content: n.containerdConfig()},
		{Path: CNIConfDir + "/10-naru.conflist", Mode: "0644", Content: string(cni) + "\n"},
		{Path: kubePath("kubelet.yaml"), Mode: "0644", Content: string(kubelet)},
		{Path: kubePath("kubelet.conf"), Mode: "0644", Content: n.kubeconfig("system:node:"+n.Name, "kubelet-client")},
		{Path: kubePath("kube-proxy.yaml"), Mode: "0644", Content: string(proxy)},
		{Path: kubePath("kube-proxy.conf"), Mode: "0644", Content: n.kubeconfig("system:kube-proxy", "kube-proxy")},
		// The kubelet's user-namespace mappings read /etc/passwd and /etc/group, which the image links here.
		{Path: kubePath("passwd"), Mode: "0644", Content: "root:x:0:0:root:/:/bin/false\n"},
		{Path: kubePath("group"), Mode: "0644", Content: "root:x:0:\n"},
		// containerd copies /etc/hosts into every Pod's sandbox, and the image links it here.
		{Path: kubePath("hosts"), Mode: "0644", Content: "127.0.0.1 localhost\n::1 localhost\n"},
	}
	if k.RegistryCA != "" {
		host := RegistryAddr
		files = append(files, file{
			Path: path.Join(CertsDir, host, "hosts.toml"), Mode: "0644",
			Content: fmt.Sprintf("server = \"https://%s\"\n\n[host.\"https://%s\"]\n  capabilities = [\"pull\", \"resolve\"]\n  ca = \"%s\"\n", host, host, k.RegistryCA),
		})
	}
	if !k.ControlPlane {
		pairs, err := n.workerPairFiles()
		if err != nil {
			return nil, err
		}
		files = append(files, pairs...)
	}
	if k.ControlPlane {
		files = append(files,
			file{Path: pkiPath("sa.key"), Mode: "0600", Content: string(k.ServiceAccountKey)},
			file{Path: pkiPath("sa.pub"), Mode: "0644", Content: string(k.ServiceAccountPub)},
			file{Path: kubePath("controller-manager.conf"), Mode: "0644", Content: n.kubeconfig("system:kube-controller-manager", "controller-manager")},
			file{Path: kubePath("scheduler.conf"), Mode: "0644", Content: n.kubeconfig("system:kube-scheduler", "scheduler")},
		)
	}
	return files, nil
}

// workerPairFiles are a worker's certificates, issued here from the cluster's authority and carried as
// files, so the Node holds the authority's certificate and not its key: the kubelet's client and
// serving pairs and kube-proxy's. init does not renew them; delivering new user-data does.
func (n Node) workerPairFiles() ([]file, error) {
	ip := net.IP(n.Address.Addr().AsSlice())
	files := []file{{Path: pkiPath("ca.crt"), Mode: "0644", Content: string(n.CA.CertPEM)}}
	for _, p := range []struct {
		name string
		pair func() (pki.Pair, error)
	}{
		{"kubelet-client", func() (pki.Pair, error) { return n.CA.Client("system:node:"+n.Name, "system:nodes") }},
		{"kubelet", func() (pki.Pair, error) { return n.CA.Server(n.Name, ip, net.IPv4(127, 0, 0, 1)) }},
		{"kube-proxy", func() (pki.Pair, error) { return n.CA.Client("system:kube-proxy") }},
	} {
		pair, err := p.pair()
		if err != nil {
			return nil, fmt.Errorf("issue %s: %w", p.name, err)
		}
		files = append(files,
			file{Path: pkiPath(p.name + ".crt"), Mode: "0644", Content: string(pair.CertPEM)},
			file{Path: pkiPath(p.name + ".key"), Mode: "0600", Content: string(pair.KeyPEM)},
		)
	}
	return files, nil
}

// containerdConfig is containerd's configuration: its stores, the CRI's network configuration and
// plugins, the sandbox image and where the registries' trust is.
func (n Node) containerdConfig() string {
	var b strings.Builder
	fmt.Fprintf(&b, "version = 3\nroot = %q\nstate = \"/run/containerd\"\n\n[grpc]\n  address = %q\n\n", ContainerdRoot, ContainerdSocket)
	fmt.Fprintf(&b, "[plugins.'io.containerd.cri.v1.runtime'.cni]\n  bin_dirs = [%q]\n  conf_dir = %q\n\n", CNIBinDir, CNIConfDir)
	// NRI would create its socket under /var/run, which the initramfs does not have, and nothing here uses it.
	b.WriteString("[plugins.'io.containerd.nri.v1.nri']\n  disable = true\n\n")
	if img := n.kube().SandboxImage; img != "" {
		fmt.Fprintf(&b, "[plugins.'io.containerd.cri.v1.images'.pinned_images]\n  sandbox = %q\n\n", img)
	}
	fmt.Fprintf(&b, "[plugins.'io.containerd.cri.v1.images'.registry]\n  config_path = %q\n", CertsDir)
	return b.String()
}
