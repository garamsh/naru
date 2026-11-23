package layout

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/wire"
)

// Where a Kubernetes Node's files are, and the ports its programs serve (ADR 0031). The API server
// serves on wire.KubeAPIPort, the port gaon's does.
const (
	// KubeDir holds the files init writes for the Kubernetes programs, and KubePKIDir the certificates.
	KubeDir    = "/run/kubernetes"
	KubePKIDir = KubeDir + "/pki"
	// ContainerdSocket is containerd's CRI endpoint, ContainerdRoot its image and snapshot store, and
	// KubeletRoot the kubelet's, both on the data disk.
	ContainerdSocket = "/run/containerd/containerd.sock"
	ContainerdRoot   = wire.DataMount + "/containerd"
	KubeletRoot      = wire.DataMount + "/kubelet"
	// CNIConfDir is where containerd reads the network configuration, and CNIBinDir the plugins the image carries.
	CNIConfDir = "/run/cni/net.d"
	CNIBinDir  = "/opt/cni/bin"
	// CertsDir is where containerd reads the trust of a registry.
	CertsDir = "/run/containerd/certs.d"
	// KubeBootstrapTokensFile is the control-plane Node's list of bootstrap tokens, which the API
	// server reads with --token-auth-file, and KubeBootstrapKubeconfig the kubeconfig a worker's
	// kubelet presents its token with. The kubelet keeps the kubeconfig it is issued, and its
	// certificates, on the data disk (ADR 0081).
	KubeBootstrapTokensFile = KubeDir + "/bootstrap-tokens.csv"
	KubeBootstrapKubeconfig = KubeDir + "/bootstrap-kubelet.conf"
	// BootstrapGroup is the group every bootstrap token authenticates as a member of.
	BootstrapGroup = "system:bootstrappers"
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
	// InClusterIssuer is the ServiceAccount issuer a cluster has when the operator sets none, and one
	// the API server accepts always (ADR 0087).
	InClusterIssuer = "https://kubernetes.default.svc." + ClusterDomain
	// JWKSPath is where, under an issuer, its JSON Web Key Set is served, as the API server serves it.
	JWKSPath = "/openid/v1/jwks"
	// ClusterDNS is the address the kubelet gives Pods as their nameserver: the tenth address of the
	// Service range, where the DNS add-on's Service is (ADR 0083).
	ClusterDNS = "10.96.0.10"

	// AddonsDir holds what the image carries of the cluster add-ons: each add-on's manifests, applied
	// by a control-plane Node, and the images, imported by every Node (ADR 0083).
	AddonsDir = "/usr/lib/naru/addons"
	// VolumesExtension is the system extension that carries the volume add-on: its driver's image and
	// manifests, too big for the initramfs (ADR 0083, 0084). It is unpinned, so init resolves it to the
	// image built for the Node's version.
	VolumesExtension = "naru-volumes"
	// VolumesConfig is the volume driver's configuration, VolumesStateDir the directory on the data
	// disk where it keeps one record per volume, and VolumesNodeLabel the label of a Node that
	// offers volumes: one with a storage disk or a block disk.
	VolumesConfig    = KubeDir + "/naru-volumes.json"
	VolumesStateDir  = wire.DataMount + "/volumes"
	VolumesNodeLabel = "naru.garam.sh/volumes"
	// storageRoot is where a storage disk is mounted, as storageRoot/<name> (config.StorageRoot).
	storageRoot = wire.DataMount + "/storage"
	// AddonManager is the field manager and label value of the objects the add-ons apply.
	AddonManager = "naru-addons"
)

// addon is one cluster add-on: its manifests are <dir>/manifests/<Name>.yaml and its image is imported from
// <dir>/images/<Image>.tar as the init service addon-image-<Image>.
type addon struct {
	Name  string
	Image string
	// Dir is where the image and manifests are; empty for AddonsDir, what the initramfs carries.
	Dir string
	// OptIn is an add-on that runs only when the cluster turns it on (KubeNode.EnabledAddons); the others run
	// unless it turns them off (KubeNode.DisabledAddons).
	OptIn bool
	// ImageWhenOff keeps the image imported when the add-on is off; a disabled volume add-on's is large, so it is not.
	ImageWhenOff bool
}

// addonTable is every cluster add-on, in the order init imports their images. The image services, the
// manifests the addons service applies, the names naructl accepts and the Kubernetes Topology all come from it.
var addonTable = []addon{
	{Name: "dns", Image: "coredns", ImageWhenOff: true},
	{Name: "volumes", Image: "volumes", Dir: path.Join("/usr/lib/naru/extensions", VolumesExtension, "addons")},
	// cloud-controller is opt-in because with it the kubelet runs with --cloud-provider=external and every Node
	// stays tainted until the controller, which needs the credential an operator makes, initializes it (ADR 0093).
	{Name: "cloud-controller", Image: "ccm", OptIn: true},
}

func addonNames(optIn bool) []string {
	var names []string
	for _, a := range addonTable {
		if a.OptIn == optIn {
			names = append(names, a.Name)
		}
	}
	return names
}

// Addons are the cluster add-ons that run unless the cluster turns them off (KubeNode.DisabledAddons).
var Addons = addonNames(false)

// OptInAddons are the cluster add-ons that run only when the cluster turns them on (KubeNode.EnabledAddons).
var OptInAddons = addonNames(true)

// addonByName is the add-on of the name; the names are checked before it is asked.
func addonByName(name string) addon {
	return addonTable[slices.IndexFunc(addonTable, func(a addon) bool { return a.Name == name })]
}

func (a addon) dir() string {
	if a.Dir == "" {
		return AddonsDir
	}
	return a.Dir
}

func (a addon) imageService() string { return "addon-image-" + a.Image }

// Kubernetes is the Topology of a Kubernetes Node: the services in the order init starts them.
var Kubernetes = Topology{
	Services: slices.Concat(
		[]Service{{Name: "registry"}, {Name: "containerd"}},
		imageServices(),
		[]Service{
			{Name: "etcd", ControlPlane: true},
			{Name: "kube-apiserver", ControlPlane: true},
			{Name: "kube-controller-manager", ControlPlane: true},
			{Name: "kube-scheduler", ControlPlane: true},
			{Name: "kube-network-policies"},
			{Name: "kubelet"},
			{Name: "kube-proxy"},
			{Name: "addons", ControlPlane: true},
		}),
	APIServer: "kube-apiserver",
	Agent:     "kubelet",
}

func imageServices() []Service {
	var out []Service
	for _, a := range addonTable {
		out = append(out, Service{Name: a.imageService()})
	}
	return out
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
// Disable apply, and API when it is set, which has init serve the Node API; the Machine, token and
// Mode fields do not.
type KubeNode struct {
	// ControlPlane makes the Node run etcd and the control-plane programs besides the kubelet.
	ControlPlane bool
	// PodNetwork is the prefix every Pod address is in, and PodPrefix the part of it this Node's Pods take.
	PodNetwork, PodPrefix netip.Prefix
	// PodPrefixFromSeed leaves PodPrefix unset and has the Node take it at boot from the naru-pod-prefix
	// of its seed's meta-data (spec.node.podPrefix, ADR 0098), which the host writes for a microVM it
	// routed a Pod prefix to. The user-data then names no Pod prefix and no Peers: the host routes every
	// member's Pod prefix, so the Node's default route reaches them.
	PodPrefixFromSeed bool
	// Peers are the other Nodes: the Node routes each one's PodPrefix via its Address.
	Peers []KubePeer
	// FrontProxy is the aggregation layer's authority, and ServiceAccountKey and ServiceAccountPub
	// the PEM of the keys of ServiceAccount tokens: all three on a control-plane Node only.
	FrontProxy                           *pki.CA
	ServiceAccountKey, ServiceAccountPub []byte
	// ServiceAccountIssuers are the issuers the operator set (ADR 0087): the first signs the tokens
	// and is the one an external provider trusts. The API server also accepts InClusterIssuer. With
	// none, InClusterIssuer signs.
	ServiceAccountIssuers []string
	// LocalRegistry makes the Node run the registry service from the data disk's inputs, and
	// RegistryCA is the path on the Node of the CA containerd trusts for RegistryAddr.
	LocalRegistry bool
	RegistryCA    string
	// SandboxImage is the image of a Pod's sandbox container.
	SandboxImage string
	// BootstrapTokens makes a control-plane Node accept bootstrap tokens, Tokens being the ones it
	// accepts when its user-data is written, and sign the kubelets' certificate requests with the
	// authority's key. A patch to the list is how a token is added or revoked (ADR 0081).
	BootstrapTokens bool
	Tokens          []string
	// Token is a worker's bootstrap token: its kubelet presents it for a client certificate in place
	// of the pair naructl would issue.
	Token string
	// DisabledAddons are the names of the add-ons (Addons) the cluster does not run: the control-plane
	// Node neither applies nor removes their objects, which the cluster's owner keeps (ADR 0083).
	DisabledAddons []string
	// EnabledAddons are the names of the opt-in add-ons (OptInAddons) the cluster runs. A control-plane Node applies
	// them with the others, and every Node's kubelet takes its Node's identity from the external cloud provider
	// when cloud-controller is among them (ADR 0093).
	EnabledAddons []string
	// BlockSerials are the serial numbers of the disks the Node offers whole as block volumes
	// (class naru-block), besides Node.Storage, which is where its local volumes are (ADR 0083).
	BlockSerials []string
	// DisableNetworkPolicy, when true, leaves kube-network-policies out of the topology: no enforcement,
	// no sysctl, no PKI pair. It is the cluster setting networkPolicy: false (ADR 0086).
	DisableNetworkPolicy bool
}

// KubeTokensContent is the content of KubeBootstrapTokensFile: a line of the API server's static
// token file for each token, which authenticates as system:bootstrap:<id> in BootstrapGroup.
func KubeTokensContent(tokens []string) string {
	var b strings.Builder
	for _, t := range tokens {
		id, _, _ := strings.Cut(t, ".")
		fmt.Fprintf(&b, "%s,system:bootstrap:%s,%s,%q\n", t, id, id, BootstrapGroup)
	}
	return b.String()
}

// BootstrapRBAC is the manifest that lets a bootstrap token's holder ask for a client certificate and
// have the controller manager approve it, and a Node renew the one it holds. The administrator
// applies it once to a cluster that takes tokens.
const BootstrapRBAC = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: naru:kubelet-bootstrap
subjects:
- kind: Group
  name: system:bootstrappers
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: system:node-bootstrapper
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: naru:approve-node-client
subjects:
- kind: Group
  name: system:bootstrappers
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: naru:renew-node-client
subjects:
- kind: Group
  name: system:nodes
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:selfnodeclient
  apiGroup: rbac.authorization.k8s.io
`

// NetworkPolicyUser is the user the NetworkPolicy enforcer's client certificate names.
const NetworkPolicyUser = "system:kube-network-policies"

// NetworkPolicyMetricsAddress is where the enforcer serves its metrics: the loopback, since the
// configuration names no port that a Node listens on beyond it (ADR 0047 item 4) and nothing scrapes it.
const NetworkPolicyMetricsAddress = "127.0.0.1:9080"

// NetworkPolicyRBACFile is where a control-plane Node carries NetworkPolicyRBAC, for the add-ons'
// service to apply.
const NetworkPolicyRBACFile = KubeDir + "/network-policy-rbac.yaml"

// NetworkPolicyRBAC is what the NetworkPolicy enforcer may do: list and watch the Pods, Namespaces
// and NetworkPolicies, which are the three informers of its v1.1.3 main (cmd/kube-network-policies/
// standard/main.go) and the rules of the upstream install.yaml. The cluster applies it itself with the
// add-ons' service whenever the enforcer is on, whichever add-ons are disabled (ADR 0094).
const NetworkPolicyRBAC = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: naru:kube-network-policies
  labels:
    app.kubernetes.io/managed-by: ` + AddonManager + `
rules:
- apiGroups: [""]
  resources: ["pods", "namespaces"]
  verbs: ["list", "watch"]
- apiGroups: ["networking.k8s.io"]
  resources: ["networkpolicies"]
  verbs: ["list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: naru:kube-network-policies
  labels:
    app.kubernetes.io/managed-by: ` + AddonManager + `
subjects:
- kind: User
  name: ` + NetworkPolicyUser + `
  apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: naru:kube-network-policies
  apiGroup: rbac.authorization.k8s.io
`

var (
	apiserverKubelet = pairSpec{"apiserver-kubelet-client", "kube-apiserver-kubelet-client"}
	frontProxy       = pairSpec{"front-proxy-client", "front-proxy-client"}
)

// memberID is the form of a group member's name, which orch gives as <group>-<number>.
var memberID = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?-[0-9]+$`)

func kubePath(name string) string { return path.Join(KubeDir, name) }

func pkiPath(name string) string { return path.Join(KubePKIDir, name) }

func (n Node) kube() KubeNode { return *n.Kube }

// checkKube reports the first thing wrong with a Kubernetes Node's description.
func (n Node) checkKube() error {
	k := n.kube()
	if err := n.checkStorage(); err != nil {
		return err
	}
	switch {
	case n.Name == "":
		return errors.New("a Node needs a name")
	case !dnsLabel.MatchString(n.Name):
		return fmt.Errorf("the name %q is not a DNS label; it is the Node's name in the cluster", n.Name)
	case n.externalCloud() && !memberID.MatchString(n.Name):
		return fmt.Errorf("the name %q is not a group member's name (<group>-<number>): with the cloud-controller add-on the kubelet's hostname is the member's name, which is how the node controller finds the member (ADR 0085 item 9)", n.Name)
	case !n.Address.IsValid() || !n.Address.Addr().Is4():
		return errors.New("the address is not an IPv4 address with a prefix length")
	case !n.APIServer.IsValid() || !n.APIServer.Is4():
		return errors.New("the API server's address is not an IPv4 address")
	case n.CA == nil:
		return errors.New("the cluster's authority is not set")
	case !k.PodNetwork.IsValid() || !k.PodNetwork.Addr().Is4() || k.PodNetwork != k.PodNetwork.Masked():
		return errors.New("the Pod network is not an IPv4 prefix in its canonical form")
	case k.PodPrefixFromSeed && (k.PodPrefix.IsValid() || len(k.Peers) > 0):
		return errors.New("a Node that takes its Pod prefix from its seed has no Pod prefix and no peers of its own: the host routes the members' Pod prefixes")
	case k.PodPrefixFromSeed:
		// The prefix is known at boot.
	case !k.PodPrefix.IsValid() || !k.PodPrefix.Addr().Is4() || k.PodPrefix != k.PodPrefix.Masked():
		return errors.New("the Pod prefix is not an IPv4 prefix in its canonical form")
	case !k.PodNetwork.Contains(k.PodPrefix.Addr()) || k.PodPrefix.Bits() < k.PodNetwork.Bits():
		return fmt.Errorf("the Pod prefix %s is not in the Pod network %s", k.PodPrefix, k.PodNetwork)
	case k.PodNetwork.Overlaps(netip.MustParsePrefix(ServiceCIDR)) || n.Address.Masked().Overlaps(k.PodNetwork):
		return fmt.Errorf("the Pod network %s overlaps the Service range %s or the Node's network %s", k.PodNetwork, ServiceCIDR, n.Address.Masked())
	case k.ControlPlane && (k.FrontProxy == nil || len(k.ServiceAccountKey) == 0 || len(k.ServiceAccountPub) == 0):
		return errors.New("a control-plane Node needs the front-proxy authority and the ServiceAccount keys")
	case k.BootstrapTokens && !k.ControlPlane, k.Token != "" && k.ControlPlane:
		return errors.New("a control-plane Node accepts bootstrap tokens and a worker presents one")
	case !k.BootstrapTokens && len(k.Tokens) > 0:
		return errors.New("tokens are for a control-plane Node that accepts bootstrap tokens")
	case k.Token != "" && !pki.ValidToken(k.Token):
		return errors.New("the bootstrap token is not <id>.<secret>, [a-z0-9]{6}.[a-z0-9]{16}")
	case !ValidIssuers(k.ServiceAccountIssuers):
		return errors.New("a ServiceAccount issuer is a plain https URL (a host and a path of letters, digits and - . _ ~ segments, no userinfo, query, fragment, trailing slash or space), and none is repeated")
	case !validAddons(k.DisabledAddons):
		return fmt.Errorf("an add-on to disable is not one of %s", strings.Join(Addons, ", "))
	case !validOptIn(k.EnabledAddons):
		return fmt.Errorf("an add-on to enable is not one of %s", strings.Join(OptInAddons, ", "))
	case !validBlockSerials(k.BlockSerials, n.Storage):
		return errors.New("a block disk's serial number is empty, repeated, or the serial of a storage disk")
	case k.RegistryCA != "" && !path.IsAbs(k.RegistryCA):
		return fmt.Errorf("the registry CA path %q is not absolute", k.RegistryCA)
	case k.RegistryCA != "" && k.SandboxImage == "":
		return errors.New("a registry CA is for the sandbox image, which is not set")
	}
	for _, t := range k.Tokens {
		if !pki.ValidToken(t) {
			return errors.New("a token in the list is not <id>.<secret>, [a-z0-9]{6}.[a-z0-9]{16}")
		}
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

// issuerArgs are the API server's ServiceAccount issuer flags: the operator's issuers in order, the
// first of which signs, then InClusterIssuer if they do not name it. With the operator's issuers the
// discovery document names the first one's key set, which is where naructl's output is served.
func (k KubeNode) issuerArgs() []string {
	issuers := slices.Clone(k.ServiceAccountIssuers)
	if !slices.Contains(issuers, InClusterIssuer) {
		issuers = append(issuers, InClusterIssuer)
	}
	var args []string
	for _, iss := range issuers {
		args = append(args, "--service-account-issuer="+iss)
	}
	if len(k.ServiceAccountIssuers) > 0 {
		args = append(args, "--service-account-jwks-uri="+JWKSURI(k.ServiceAccountIssuers[0]))
	}
	return args
}

// JWKSURI is where issuer serves its JSON Web Key Set.
func JWKSURI(issuer string) string { return issuer + JWKSPath }

// ValidIssuers reports whether every issuer is a plain https URL, as OpenID Connect Discovery requires
// of an issuer, and none is repeated. Plain is: the scheme https in lower case, a host, no userinfo,
// query or fragment, and a path of segments of letters, digits and - . _ ~ that are neither empty (so
// no trailing slash) nor "." nor "..". A path is also a directory name under gen kubernetes-jwks's
// --out-dir, and a line of a file, which is why it is this narrow.
func ValidIssuers(issuers []string) bool {
	seen := map[string]bool{}
	for _, iss := range issuers {
		if !plainIssuer(iss) || seen[iss] {
			return false
		}
		seen[iss] = true
	}
	return true
}

func plainIssuer(iss string) bool {
	rest, ok := strings.CutPrefix(iss, "https://")
	if !ok {
		return false
	}
	host, path, _ := strings.Cut(rest, "/")
	if host == "" || strings.ContainsFunc(host, func(r rune) bool { return r <= ' ' || r >= 0x7f || strings.ContainsRune("@?#%\\/", r) }) {
		return false
	}
	if u, err := url.Parse(iss); err != nil || u.Host != host {
		return false
	}
	if !strings.Contains(rest, "/") {
		return true
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsFunc(seg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r))
		}) {
			return false
		}
	}
	return true
}

// validAddons reports whether every name is an add-on.
func validAddons(names []string) bool {
	return !slices.ContainsFunc(names, func(name string) bool { return !slices.Contains(Addons, name) })
}

// validOptIn reports whether every name is an opt-in add-on.
func validOptIn(names []string) bool {
	return !slices.ContainsFunc(names, func(name string) bool { return !slices.Contains(OptInAddons, name) })
}

// validBlockSerials reports whether the serials are distinct, non-empty, and none is a storage
// disk's: init formats and mounts a storage disk, and the driver never touches a block disk's content.
func validBlockSerials(serials []string, storage []StorageDisk) bool {
	seen := map[string]bool{}
	for _, s := range serials {
		if s == "" || seen[s] || slices.ContainsFunc(storage, func(d StorageDisk) bool { return d.Serial == s }) {
			return false
		}
		seen[s] = true
	}
	return true
}

// offersVolumes reports whether the Node runs the volume driver's DaemonSet pod: it has a disk to offer.
func (n Node) offersVolumes() bool {
	return slices.Contains(n.enabledAddons(), "volumes") && (len(n.Storage) > 0 || len(n.kube().BlockSerials) > 0)
}

// volumesConfig is the content of VolumesConfig.
func (n Node) volumesConfig() (string, error) {
	c := struct {
		StateDir     string   `json:"stateDir"`
		StorageDir   string   `json:"storageDir,omitempty"`
		BlockSerials []string `json:"blockSerials,omitempty"`
	}{StateDir: VolumesStateDir, BlockSerials: n.kube().BlockSerials}
	if len(n.Storage) > 0 {
		c.StorageDir = path.Join(storageRoot, n.Storage[0].Name)
	}
	b, err := json.Marshal(c)
	return string(b) + "\n", err
}

// appliesObjects reports whether the control-plane Node runs the service that applies objects to the
// cluster: when an add-on is enabled, and when the NetworkPolicy enforcer is on, which needs its RBAC.
func (n Node) appliesObjects() bool {
	return len(n.enabledAddons()) > 0 || !n.kube().DisableNetworkPolicy
}

// enabledAddons are the add-ons the Node's cluster runs: those of Addons it did not disable, then the opt-in
// ones it enabled, each in its list's order.
func (n Node) enabledAddons() []string {
	on := slices.DeleteFunc(slices.Clone(Addons), func(name string) bool { return slices.Contains(n.kube().DisabledAddons, name) })
	return append(on, slices.DeleteFunc(slices.Clone(OptInAddons), func(name string) bool { return !slices.Contains(n.kube().EnabledAddons, name) })...)
}

// externalCloud reports whether the cluster runs the cloud-controller add-on, so that every kubelet runs with
// an external cloud provider.
func (n Node) externalCloud() bool { return slices.Contains(n.enabledAddons(), "cloud-controller") }

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
	c.Spec.DataDisk.Label = wire.DataLabel
	c.Spec.DataDisk.MountPoint = wire.DataMount
	// A Kubernetes Node may be a microVM, whose host gives it a data disk it cannot format; a disk
	// already labelled is left as it is (docs/architecture/init.md).
	c.Spec.DataDisk.FormatBlank = true
	c.Spec.Network.Forwarding = []string{"ipv4"}
	if !k.DisableNetworkPolicy {
		c.Spec.Network.BridgeNetfilter = true
	}
	// init writes the bridge CNI configuration from spec.node (config.CNIConflist) for every Node; the
	// Pod prefix is named here unless the seed's meta-data gives it.
	c.Spec.Node = &nodeSpec{CNIConf: CNIConfDir + "/10-naru.conflist"}
	if !k.PodPrefixFromSeed {
		c.Spec.Node.PodPrefix = k.PodPrefix.String()
	}
	for _, p := range k.Peers {
		c.Spec.Network.Routes = append(c.Spec.Network.Routes, route{To: p.PodPrefix, Via: p.Address})
	}
	for _, d := range n.Storage {
		c.Spec.Storage = append(c.Spec.Storage, storage{Name: d.Name, Label: d.Label, Device: d.Device, Serial: d.Serial})
	}
	if slices.Contains(n.enabledAddons(), "volumes") {
		c.Spec.Extensions = append(c.Spec.Extensions, extensionSpec{Name: VolumesExtension})
	}
	files, err := n.kubeFiles()
	if err != nil {
		return nil, err
	}
	c.Spec.Files = files
	c.Spec.PKI = n.kubePKI()
	if c.Spec.API, err = n.specAPI(); err != nil {
		return nil, fmt.Errorf("issue the Node API's serving pair: %w", err)
	}
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
				"--advertise-address=" + n.APIServer.String(), "--secure-port=" + wire.KubeAPIPort,
				"--etcd-servers=" + n.etcdEndpoints(), "--etcd-cafile=" + gaonPath("ca.pem"),
				"--etcd-certfile=" + gaonPath(apiserverTo.name+".pem"), "--etcd-keyfile=" + gaonPath(apiserverTo.name+".key"),
				"--client-ca-file=" + ca, "--tls-cert-file=" + pkiPath("apiserver.crt"), "--tls-private-key-file=" + pkiPath("apiserver.key"),
				"--kubelet-certificate-authority=" + ca, "--kubelet-client-certificate=" + pkiPath(apiserverKubelet.name+".crt"),
				"--kubelet-client-key=" + pkiPath(apiserverKubelet.name+".key"), "--kubelet-preferred-address-types=InternalIP",
				"--service-cluster-ip-range=" + ServiceCIDR,
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
			Name: "kube-network-policies", Binary: n.binary("kube-network-policies", "/bin/kube-network-policies"), Env: n.kubeEnv(),
			// The Pod's IP comes from the API server: NRI is off in containerd (ADR 0031 item 10).
			Args: []string{"--kubeconfig=" + kubePath("kube-network-policies.conf"), "--hostname-override=" + n.Name, "--disable-nri",
				// The agent fails open and serves metrics on every address unless told otherwise.
				"--fail-open=false", "--metrics-bind-address=" + NetworkPolicyMetricsAddress},
		},
		{
			Name: "kubelet", Binary: n.binary("kubelet", "/bin/kubelet"), Env: n.kubeEnv(),
			Args: []string{
				"--config=" + kubePath("kubelet.yaml"), "--kubeconfig=" + n.kubeletKubeconfig(), "--hostname-override=" + n.Name,
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
	if k.DisableNetworkPolicy {
		list = slices.DeleteFunc(list, func(s service) bool { return s.Name == "kube-network-policies" })
	}
	enabled := n.enabledAddons()
	images := n.addonImageServices(enabled)
	at := slices.IndexFunc(list, func(s service) bool { return s.Name == "etcd" })
	list = slices.Insert(list, at, images...)
	if k.ControlPlane && n.appliesObjects() {
		args := []string{
			"apply", "--kubeconfig=" + kubePath("addons.conf"), "--cache-dir=" + kubePath("addons-cache"),
			"--server-side", "--force-conflicts", "--field-manager=" + AddonManager,
		}
		if !k.DisableNetworkPolicy {
			args = append(args, "-f", NetworkPolicyRBACFile)
		}
		for _, name := range enabled {
			args = append(args, "-f", path.Join(addonByName(name).dir(), "manifests", name+".yaml"))
		}
		list = append(list, service{Name: "addons", Binary: n.binary("addons", "/bin/kubectl"), Args: args, RestartPolicy: restartOnFailure})
	}
	for i := range list {
		if list[i].Name == "kube-apiserver" {
			list[i].Args = append(list[i].Args, k.issuerArgs()...)
		}
		switch {
		case list[i].Name == "kube-apiserver" && k.BootstrapTokens:
			list[i].Args = append(list[i].Args, "--token-auth-file="+KubeBootstrapTokensFile)
		case list[i].Name == "kube-controller-manager" && k.BootstrapTokens:
			list[i].Args = append(list[i].Args, "--cluster-signing-cert-file="+ca, "--cluster-signing-key-file="+pkiPath("ca.key"))
		case list[i].Name == "kubelet" && k.Token != "":
			list[i].Args = append(list[i].Args, "--bootstrap-kubeconfig="+KubeBootstrapKubeconfig, "--cert-dir="+KubeletRoot+"/pki")
		}
		if list[i].Name == "kubelet" && n.externalCloud() {
			list[i].Args = append(list[i].Args, "--cloud-provider=external")
		}
		if list[i].Name == "kubelet" && n.offersVolumes() {
			list[i].Args = append(list[i].Args, "--node-labels="+VolumesNodeLabel+"=true")
		}
	}
	for i := range list {
		list[i].ControlPlane = Kubernetes.OnlyControlPlane(list[i].Name)
	}
	return list
}

// restartOnFailure is the restart policy of a service that runs until it succeeds.
const restartOnFailure = "on-failure"

// ctrImportArgs are the arguments of ctr, containerd's client, that import the image of add-on a from
// AddonsDir/images into the namespace the CRI reads and pin it. The Node names nothing: the archive names
// its image by the reference the manifests use (ADR 0105), and ctr takes that name as it is.
func ctrImportArgs(a addon) []string {
	return []string{
		"--address", ContainerdSocket, "--namespace", "k8s.io", "images", "import",
		"--label", "io.cri-containerd.pinned=pinned", path.Join(a.dir(), "images", a.Image+".tar"),
	}
}

// addonImageServices are the services that import the images of the add-ons that are enabled, and of those
// whose image stays imported when they are off, in the table's order.
func (n Node) addonImageServices(enabled []string) []service {
	var out []service
	for _, a := range addonTable {
		if slices.Contains(enabled, a.Name) || a.ImageWhenOff {
			out = append(out, service{Name: a.imageService(), Binary: n.binary(a.imageService(), "/bin/ctr"), RestartPolicy: restartOnFailure, Args: ctrImportArgs(a)})
		}
	}
	return out
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
	if k.BootstrapTokens {
		spec.Authorities[0].KeyPath = pkiPath("ca.key")
	}
	add := func(c ...pkiCertificate) { spec.Certificates = append(spec.Certificates, c...) }
	spec.Authorities = append(spec.Authorities,
		pkiAuthority{Name: "etcd", Certificate: string(n.CA.CertPEM), Key: string(n.CA.KeyPEM), CertificatePath: gaonPath("ca.pem")},
		pkiAuthority{Name: "front-proxy", Certificate: string(k.FrontProxy.CertPEM), Key: string(k.FrontProxy.KeyPEM), CertificatePath: pkiPath("front-proxy-ca.crt")},
	)
	apiserver := cluster(pairSpec{"apiserver", "kube-apiserver"}, "server", "kube-apiserver")
	apiserver.NodeAddresses = true
	apiserver.IPAddresses = []string{n.APIServer.String(), firstHost(ServiceCIDR)}
	// ServingAddresses are the addresses a client dials the API server at that are not the Node's own,
	// such as the LAN address a host leases to it (ADR 0102); the certificate is signed before the Node
	// exists, so they are known beforehand.
	for _, a := range n.ServingAddresses {
		if !slices.Contains(apiserver.IPAddresses, a.String()) {
			apiserver.IPAddresses = append(apiserver.IPAddresses, a.String())
		}
	}
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
	// The add-ons' pair is the cluster's administrator, as the API server's kubelet client is: the
	// add-ons create cluster roles and bindings. It is not renewed under a running apply.
	if n.appliesObjects() {
		addons := cluster(pairSpec{"addons", "naru:addons"}, "client", "addons")
		addons.Organization = "system:masters"
		add(addons)
	}
	node := cluster(pairSpec{"kubelet-client", "system:node:" + n.Name}, "client", "kubelet")
	node.Organization = "system:nodes"
	serving := cluster(pairSpec{"kubelet", n.Name}, "server", "kubelet")
	serving.NodeAddresses = true
	serving.DNSNames = []string{n.Name}
	add(node, serving)
	add(cluster(pairSpec{"kube-proxy", "system:kube-proxy"}, "client", "kube-proxy"))
	if !k.DisableNetworkPolicy {
		add(cluster(pairSpec{"kube-network-policies", NetworkPolicyUser}, "client", "kube-network-policies"))
	}
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
`, net.JoinHostPort(n.APIServer.String(), wire.KubeAPIPort), pkiPath("ca.crt"), user, pkiPath(pair+".crt"), pkiPath(pair+".key"), user)
}

// kubeletKubeconfig is the kubeconfig the kubelet dials with. A kubelet given a token makes it, on the
// data disk, from its bootstrap kubeconfig; any other has the one the Node's user-data carries.
func (n Node) kubeletKubeconfig() string {
	if n.kube().Token != "" {
		return KubeletRoot + "/kubelet.conf"
	}
	return kubePath("kubelet.conf")
}

// bootstrapKubeconfig is the kubeconfig a kubelet presents its token with.
func (n Node) bootstrapKubeconfig() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: naru
  cluster:
    server: https://%s
    certificate-authority: %s
users:
- name: kubelet-bootstrap
  user:
    token: %s
contexts:
- name: default
  context:
    cluster: naru
    user: kubelet-bootstrap
current-context: default
`, net.JoinHostPort(n.APIServer.String(), wire.KubeAPIPort), pkiPath("ca.crt"), n.kube().Token)
}

// kubeFiles are what init writes for the Kubernetes programs: their configuration, on a worker its
// pairs, and on a control-plane Node the ServiceAccount keys, which are not issued at boot.
func (n Node) kubeFiles() ([]file, error) {
	k := n.kube()
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
		"clusterDNS":               []string{ClusterDNS},
		"clusterDomain":            ClusterDomain,
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
		{Path: kubePath("kubelet.yaml"), Mode: "0644", Content: string(kubelet)},
		{Path: kubePath("kube-proxy.yaml"), Mode: "0644", Content: string(proxy)},
		{Path: kubePath("kube-proxy.conf"), Mode: "0644", Content: n.kubeconfig("system:kube-proxy", "kube-proxy")},
	}
	if !k.DisableNetworkPolicy {
		files = append(files, file{Path: kubePath("kube-network-policies.conf"), Mode: "0644", Content: n.kubeconfig(NetworkPolicyUser, "kube-network-policies")})
	}
	files = append(files,
		// The kubelet's user-namespace mappings read /etc/passwd and /etc/group, which the image links here.
		file{Path: kubePath("passwd"), Mode: "0644", Content: "root:x:0:0:root:/:/bin/false\n"},
		file{Path: kubePath("group"), Mode: "0644", Content: "root:x:0:\n"},
		// containerd copies /etc/hosts into every Pod's sandbox, and the image links it here.
		file{Path: kubePath("hosts"), Mode: "0644", Content: "127.0.0.1 localhost\n::1 localhost\n"},
	)
	if k.RegistryCA != "" {
		host := RegistryAddr
		files = append(files, file{
			Path: path.Join(CertsDir, host, "hosts.toml"), Mode: "0644",
			Content: fmt.Sprintf("server = \"https://%s\"\n\n[host.\"https://%s\"]\n  capabilities = [\"pull\", \"resolve\"]\n  ca = \"%s\"\n", host, host, k.RegistryCA),
		})
	}
	if k.Token != "" {
		files = append(files, file{Path: KubeBootstrapKubeconfig, Mode: "0600", Content: n.bootstrapKubeconfig()})
	} else {
		files = append(files, file{Path: kubePath("kubelet.conf"), Mode: "0644", Content: n.kubeconfig("system:node:"+n.Name, "kubelet-client")})
	}
	if !k.ControlPlane {
		pairs, err := n.workerPairFiles()
		if err != nil {
			return nil, err
		}
		files = append(files, pairs...)
	}
	if n.offersVolumes() {
		cfg, err := n.volumesConfig()
		if err != nil {
			return nil, err
		}
		files = append(files, file{Path: VolumesConfig, Mode: "0644", Content: cfg})
	}
	if k.ControlPlane {
		files = append(files,
			file{Path: pkiPath("sa.key"), Mode: "0600", Content: string(k.ServiceAccountKey), Restart: []string{Kubernetes.APIServer, "kube-controller-manager"}},
			file{Path: pkiPath("sa.pub"), Mode: "0644", Content: string(k.ServiceAccountPub), Restart: []string{Kubernetes.APIServer}},
			file{Path: kubePath("controller-manager.conf"), Mode: "0644", Content: n.kubeconfig("system:kube-controller-manager", "controller-manager")},
			file{Path: kubePath("scheduler.conf"), Mode: "0644", Content: n.kubeconfig("system:kube-scheduler", "scheduler")},
		)
		if n.appliesObjects() {
			files = append(files, file{Path: kubePath("addons.conf"), Mode: "0644", Content: n.kubeconfig("naru:addons", "addons")})
		}
		if !k.DisableNetworkPolicy {
			files = append(files, file{Path: NetworkPolicyRBACFile, Mode: "0644", Content: NetworkPolicyRBAC})
		}
		if k.BootstrapTokens {
			files = append(files, file{Path: KubeBootstrapTokensFile, Mode: "0600", Content: KubeTokensContent(k.Tokens), Restart: []string{Kubernetes.APIServer}})
		}
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
		{"kube-network-policies", func() (pki.Pair, error) {
			return n.CA.Client(NetworkPolicyUser)
		}},
	} {
		if p.name == "kubelet-client" && n.kube().Token != "" {
			continue
		}
		if p.name == "kube-network-policies" && n.kube().DisableNetworkPolicy {
			continue
		}
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
