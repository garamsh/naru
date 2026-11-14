package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// OverrideDir is where the data disk carries the gaon binaries a harness supplies, as a Node sees
// it: the paths naructl is told through gen node --binary.
const OverrideDir = fixture.GuestRoot + "/gaon"

// ClusterSpec says what N Nodes on one network boot.
type ClusterSpec struct {
	// N is the number of Nodes; their names are node-1 to node-N.
	N int
	// Disk is the image's naru.img, which the Nodes boot under OVMF.
	Disk string
	// Dir receives one directory per Node.
	Dir string
	// Inputs is the staged tree every Node's data disk carries.
	Inputs fixture.Inputs
	// ProbeOn is the number of the Node that runs the probe, or 0 for none.
	ProbeOn int
	// Gaon runs gaon on the Nodes, with the user-data and the certificates Naructl writes (ADR 0015):
	// the Node Agent on each, and etcd, the API server, the scheduler and the controllers on Node 1.
	Gaon bool
	// Naructl is the naructl binary, which Gaon requires.
	Naructl naructl.Tool
	// BootPKI, with Gaon, has Node 1 issue its certificates from the CA its spec.pki carries and sign
	// the agents' requests with it, and every Node's agent get its pair through a bootstrap token
	// (ADR 0013, ADR 0016). Without it every pair of every Node is an operator's file.
	BootPKI bool
	// Disable names services left out of every Node's configuration; naructl refuses a name it
	// does not know.
	Disable []string
	// GaonBinaries names the gaon services whose binary the Inputs tree carries under gaon/.
	GaonBinaries []string
	// NodeAPI, with Gaon, has `naructl gen api-ca` write the Node API's authority, so that every Node's
	// user-data carries spec.api, and node-1's API is reachable from the host through a forward.
	NodeAPI bool
	// Host, with Gaon, has `naructl gen host-ca` write the host API's authority, so that node-1's
	// user-data carries spec.host (region HostRegion, labels HostLabels) and runs naru-hostd, and
	// node-1's host API is reachable from the host through a forward. node-1 also runs naru-mvmdlist,
	// which prints the Machines its mvmd holds (ADR 0028).
	Host bool
	// Hosts, with Host, are the numbers of the Nodes that run naru-hostd, each with a host API of its
	// own, the same host authority, a route and a second NIC with forwards. Empty is Node 1 alone.
	// A Node that is no control-plane Node still runs Gaon's Node Agent (docs/architecture/harness.md).
	Hosts []int
	// Forward, with Gaon, is guest ports of the second NIC that every Node has forwarded from a
	// loopback port of the test host (Cluster.Forwards): orchd's 7500, a socket proxy, the metrics.
	Forward []string
	// Capture records the shared network at every Node (Spec.Capture, Node.CapturePath).
	Capture bool
	// TimeServers, with Gaon, is node-1's spec.time.servers. The servers are on the test host, which
	// node-1 reaches at 10.0.2.2 over its second NIC, left unrestricted for it (Config.HostReach).
	TimeServers []string
	// ControlPlanes, with Gaon, is the number of Nodes, the first ones, that are control-plane Nodes.
	// Zero or one is Node 1 alone. More is a control plane of that many members (ADR 0024): each
	// has etcd, the API server, the fence and the holder of VirtualAddress, which every Node's
	// agent dials, and each has the second NIC with a host forward of its own.
	ControlPlanes int
	// EtcdProbeOn, with ControlPlanes above one, is the number of the Node that also runs naru-etcdprobe,
	// which prints what a client of the control plane sees from a member's address and from one that
	// is no member's, or 0 for none.
	EtcdProbeOn int
	// SerialSocket gives every Node's serial port a socket nothing reads (Spec.SerialSocket).
	SerialSocket bool
	// ExtraServices, with Gaon, are services appended to every Node's user-data, after naructl's.
	ExtraServices []ExtraService
	// MemoryMiB, when not 0, is every Node's memory in place of the default (Config.MemoryMiB): a host
	// that runs a Kubernetes Node as a microVM needs the microVM's memory and its own.
	MemoryMiB int
	// HostMemoryMiB, when not 0, is the memory of the Nodes that run naru-hostd in place of MemoryMiB:
	// a fleet's control Nodes keep the default while a host adds its microVMs' memory to it.
	HostMemoryMiB int
	// RTCBase, when not empty, is every Node's -rtc base, so that the Nodes boot with a wrong clock.
	RTCBase string
	// Registries are the gen node --registry values of every Node of a Gaon cluster. When nil, each
	// Node trusts the registry service's authority for RegistryAddr through its configuration.
	Registries []string
	// AWSRegion, AWSTrustAnchor, AWSProfile and AWSRole, with Gaon, are every Node's gen node --aws-*
	// flags, all or none: the ARNs of a Roles Anywhere profile (fake ones, in a test).
	AWSRegion, AWSTrustAnchor, AWSProfile, AWSRole string
	// EditUserData, when set, changes the user-data naructl wrote for a Gaon Node before the Node
	// starts: the test adds files, such as a credential helper, or a service's arguments.
	EditUserData func(c Config, file string) error
	// GenFlags, when set, gives gen node further flags for the Node c names: the sections of spec that
	// naructl has a flag for (--signer, --metrics-listen, --host-management-prefix).
	GenFlags func(c Config) []string
	// Configure, when set, changes each Node's Config before it starts, such as to give it routes.
	// It does not apply to a Gaon cluster, whose user-data is naructl's.
	Configure func(*Config)
}

// Cluster is the Nodes of one network. Node 1 is the one whose configuration
// sets spec.controlPlane.
type Cluster struct {
	Nodes []*Node
	// Client and HostAddr are set when the cluster runs gaon: the test host's client pair with the
	// CA that signed it, and the address on the test host, "127.0.0.1:port", that reaches the API server.
	Client   naructl.Client
	HostAddr string
	// HostAddrs is the host side of each control-plane Node's forward to its API server, by Node name.
	HostAddrs map[string]string
	// API is set when the cluster has NodeAPI.
	API *NodeAPI
	// Host is set when the cluster has Host: node-1's, or nil when node-1 is no host.
	Host *HostAPI
	// Hosts is the host API of each Node that runs naru-hostd, in the order of ClusterSpec.Hosts.
	Hosts []*HostAPI
	// HostCA is the certificate file of the authority the hosts' API certificates chain to, which orch
	// trusts for them (spec.orch.hostCA). It is set when the cluster has hosts.
	HostCA string
	// Forwards is, by Node name and guest port, the "127.0.0.1:port" of each ClusterSpec.Forward.
	Forwards map[string]map[string]string
}

// HostRegion and HostLabels are what a cluster with Host configures node-1's spec.host with.
const HostRegion = "eu-1"

// HostLabels are the labels of node-1's spec.host before a test changes them.
var HostLabels = []string{"disk=ssd"}

// HostAPI is how the test host reaches node-1's host API: the address of the forward and naructl
// client files, each naructl's own format (ADR 0028).
type HostAPI struct {
	// Node is the name of the Node that runs it, and Index its number.
	Node  string
	Index int
	// Addr is "127.0.0.1:port", the host side of the forward to node-1's host API.
	Addr string
	// Provisioner and Reader are client files with the certificate of that role from the cluster's
	// host authority. Foreign is Provisioner's file with the certificate and key of a provisioner of
	// another authority: the server's certificate is still trusted, and the client's is not.
	Provisioner, Reader, Foreign string
	// Route is "127.0.0.1:port", the host side of the forward to node-1's naru-mvmroute: the route
	// to the addresses of the microVMs on node-1's Machine prefix (see Cluster.RouteTo).
	Route string
}

// NodeAPI is how the test host reaches node-1's Node API: the address of the forward and naructl
// client files, each naructl's own format (ADR 0017).
type NodeAPI struct {
	// Addr is "127.0.0.1:port", the host side of the forward to node-1's Node API.
	Addr string
	// Admin and Reader are client files with the certificate of that role from the cluster's Node API
	// authority. Foreign is Admin's file with the certificate and key of an admin of another
	// authority: the server's certificate is still trusted, and the client's is not.
	Admin, Reader, Foreign string
}

// StartCluster boots N Nodes that share a network and returns them as soon as
// QEMU has started each. The caller must Stop it.
func StartCluster(spec ClusterSpec) (*Cluster, error) {
	if spec.N < 1 {
		return nil, errors.New("a cluster needs at least one Node")
	}
	if spec.ControlPlanes > spec.N || spec.ControlPlanes > 1 && !spec.Gaon {
		return nil, errors.New("ControlPlanes needs Gaon and no more Nodes than the cluster has")
	}
	if spec.Host && !spec.Gaon {
		return nil, errors.New("Host needs Gaon: the user-data with spec.host is naructl's")
	}
	if spec.NodeAPI && !spec.Gaon {
		return nil, errors.New("NodeAPI needs Gaon: the user-data with spec.api is naructl's")
	}
	// The Nodes of one cluster meet on a multicast group and port of their own, so that
	// clusters running side by side on one host do not share a network.
	mcast := fmt.Sprintf("230.78.0.1:%d", 20000+rand.IntN(40000)) //nolint:gosec // a port, not a secret
	c := &Cluster{}
	var gen *gaonFiles
	if spec.Gaon {
		var err error
		if gen, err = newGaonFiles(spec); err != nil {
			return nil, err
		}
		c.Client, c.HostAddr, c.API = gen.client, "127.0.0.1:"+strconv.Itoa(gen.ports[1]), gen.api
		c.Hosts, c.Forwards = gen.hostList, gen.forwarded
		if len(gen.hostList) > 0 {
			// The authority's directory goes when StartCluster returns; the certificate stays.
			c.HostCA = filepath.Join(spec.Dir, "host-ca.pem")
			ca, err := os.ReadFile(filepath.Join(gen.caDir, "host", "ca.pem"))
			if err != nil {
				return nil, fmt.Errorf("read the host authority's certificate: %w", err)
			}
			if err := os.WriteFile(c.HostCA, ca, 0o644); err != nil { //nolint:gosec // a certificate holds nothing secret
				return nil, err
			}
		}
		if h := gen.hosts[1]; h != nil {
			c.Host = h.api
		}
		c.HostAddrs = map[string]string{}
		for i, p := range gen.ports {
			if spec.controlPlane(i) {
				c.HostAddrs[fmt.Sprintf("node-%d", i)] = "127.0.0.1:" + strconv.Itoa(p)
			}
		}
		// The CA's key stays on disk only while naructl needs it, whichever way StartCluster returns.
		defer func() { _ = gen.dropCA() }()
	}
	for i := 1; i <= spec.N; i++ {
		name := fmt.Sprintf("node-%d", i)
		cfg := Config{
			Name: name, Index: i, Total: spec.N, ControlPlane: spec.controlPlane(i), Probe: i == spec.ProbeOn, Inputs: spec.Inputs, MemoryMiB: spec.MemoryMiB,
		}
		if spec.HostMemoryMiB > 0 && spec.isHost(i) {
			cfg.MemoryMiB = spec.HostMemoryMiB
		}
		if !spec.Gaon {
			ca, err := os.ReadFile(filepath.Join(spec.Inputs.Dir, "registry", "ca.pem"))
			if err != nil {
				return nil, fmt.Errorf("read the registry's authority: %w", err)
			}
			cfg.RegistryCA = string(ca)
		}
		if spec.Configure != nil {
			spec.Configure(&cfg)
		}
		start := Spec{Config: cfg, Disk: spec.Disk, Dir: filepath.Join(spec.Dir, name), Mcast: mcast, RTCBase: spec.RTCBase, SerialSocket: spec.SerialSocket, Capture: spec.Capture}
		if gen != nil {
			if spec.secondNIC(i) {
				start.Config.HostPort = gen.ports[i] // a control-plane Node and a host have the second NIC
				start.Config.ExtraForwards = gen.extraForwards[i]
				if h := gen.hosts[i]; h != nil {
					start.Config.HostAPIHostPort = h.port
					start.Config.RouteHostPort = h.routePort
				}
				if i == 1 {
					start.Config.APIHostPort = gen.apiPort
					start.Config.HostReach = len(spec.TimeServers) > 0
				}
			}
			var err error
			if start.NaruYAML, start.NetworkConfig, err = gen.node(start.Config, spec); err != nil {
				c.Stop()
				return nil, fmt.Errorf("write the user-data of %s: %w", name, err)
			}
		}
		n, err := Start(start)
		if err != nil {
			c.Stop()
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
		c.Nodes = append(c.Nodes, n)
	}
	return c, nil
}

// ExtraService is a test's own service: a name and the guest path of a program the Inputs tree carries.
type ExtraService struct {
	Name, Binary string
	Args         []string
}

// controlPlane reports whether Node i is a control-plane Node.
func (spec ClusterSpec) controlPlane(i int) bool { return i <= max(spec.ControlPlanes, 1) }

// hostNodes is the numbers of the Nodes that run naru-hostd.
func (spec ClusterSpec) hostNodes() []int {
	switch {
	case !spec.Host:
		return nil
	case len(spec.Hosts) == 0:
		return []int{1}
	}
	return spec.Hosts
}

// isHost reports whether Node i runs naru-hostd.
func (spec ClusterSpec) isHost(i int) bool { return slices.Contains(spec.hostNodes(), i) }

// secondNIC reports whether Node i has the second NIC: a control-plane Node, a host, and every Node
// when the cluster has forwards of its own.
func (spec ClusterSpec) secondNIC(i int) bool {
	return spec.controlPlane(i) || spec.isHost(i) || len(spec.Forward) > 0
}

// ha reports whether the control plane has several members.
func (spec ClusterSpec) ha() bool { return spec.ControlPlanes > 1 }

// Stop ends every Node.
func (c *Cluster) Stop() {
	for _, n := range c.Nodes {
		n.Stop()
	}
}

// freePort returns a TCP port on the loopback that nothing listens on now.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// gaonFiles are what naructl wrote for a gaon cluster: the CA, the test host's client pair and the
// guest manifest the Nodes' mvmd configuration is made from.
type gaonFiles struct {
	caDir, guest string
	client       naructl.Client
	// etcdClient is the directory naructl gen etcd-client wrote, set when the control plane has
	// several members.
	etcdClient string
	// ports are the host ports forwarded to the API server of each control-plane Node, by number.
	ports map[int]int
	// api and apiPort are set when the cluster has a Node API.
	api     *NodeAPI
	apiPort int
	// hosts is the host API of each host Node by number, and hostList the same in order.
	hosts    map[int]*hostFiles
	hostList []*HostAPI
	// extraForwards is the ClusterSpec.Forward of each Node by number, and forwarded the same by name.
	extraForwards map[int][]Forward
	forwarded     map[string]map[string]string
}

// hostFiles is a host's API and the host ports forwarded to its host API and to its naru-mvmroute.
type hostFiles struct {
	api             *HostAPI
	port, routePort int
}

func newGaonFiles(spec ClusterSpec) (*gaonFiles, error) {
	g := &gaonFiles{caDir: filepath.Join(spec.Dir, "ca"), guest: filepath.Join(spec.Dir, "guest.json"), ports: map[int]int{},
		hosts: map[int]*hostFiles{}, extraForwards: map[int][]Forward{}, forwarded: map[string]map[string]string{}}
	for i := 1; i <= spec.N; i++ {
		if !spec.secondNIC(i) {
			continue
		}
		port, err := freePort()
		if err != nil {
			return nil, fmt.Errorf("pick a host port for the API server: %w", err)
		}
		g.ports[i] = port
		name := fmt.Sprintf("node-%d", i)
		g.forwarded[name] = map[string]string{}
		for _, guest := range spec.Forward {
			fp, err := freePort()
			if err != nil {
				return nil, fmt.Errorf("pick a host port for the forward to %s: %w", guest, err)
			}
			g.extraForwards[i] = append(g.extraForwards[i], Forward{HostPort: fp, GuestPort: guest})
			g.forwarded[name][guest] = "127.0.0.1:" + strconv.Itoa(fp)
		}
	}
	manifest, err := json.Marshal(spec.Inputs.Guest)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil { //nolint:gosec // a test directory
		return nil, err
	}
	if err := os.WriteFile(g.guest, manifest, 0o600); err != nil {
		return nil, err
	}
	if err := spec.Naructl.GenCA(g.caDir); err != nil {
		return nil, err
	}
	clientDir := filepath.Join(spec.Dir, "client")
	if g.client, err = spec.Naructl.GenClient(g.caDir, "naru-test-host", clientDir); err != nil {
		return nil, err
	}
	if spec.ha() {
		g.etcdClient = filepath.Join(spec.Dir, "etcd-client")
		if err := spec.Naructl.GenEtcdClient(g.caDir, g.etcdClient); err != nil {
			return nil, err
		}
	}
	if spec.BootPKI {
		// Node 1's user-data, written first, lists the token of every Node.
		for i := 1; i <= spec.N; i++ {
			if err := spec.Naructl.GenToken(g.caDir, fmt.Sprintf("node-%d", i)); err != nil {
				return nil, err
			}
		}
	}
	if spec.NodeAPI {
		if err := g.nodeAPIFiles(spec); err != nil {
			return nil, err
		}
	}
	if spec.Host {
		if err := g.hostAPIFiles(spec); err != nil {
			return nil, err
		}
	}
	// The CA's directory is removed once the last Node's user-data is written, key and all, so the
	// client keeps its own copy of the certificate.
	cert, err := os.ReadFile(g.client.CA)
	if err != nil {
		return nil, err
	}
	g.client.CA = filepath.Join(clientDir, "ca.pem")
	if err := os.WriteFile(g.client.CA, cert, 0o644); err != nil { //nolint:gosec // a certificate
		return nil, err
	}
	return g, nil
}

// dropCA removes the CA's directory: no file under -dir holds the CA's own key once the Nodes'
// user-data is written, except a control-plane Node's user-data that issues its certificates at boot.
func (g *gaonFiles) dropCA() error { return os.RemoveAll(g.caDir) }

// node has naructl write the user-data and network-config of the Node c names, on the shared
// network. The serving certificate names the Nodes' address and the host forward's.
func (g *gaonFiles) node(c Config, spec ClusterSpec) (userData, networkConfig []byte, err error) {
	n := c.net()
	ones, _ := net.IPMask(net.ParseIP(n.Netmask).To4()).Size()
	dir := filepath.Join(spec.Dir, c.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
		return nil, nil, err
	}
	a := naructl.Node{
		CA: g.caDir, Name: c.Name, ControlPlane: c.ControlPlane, PairsAsFiles: !spec.BootPKI,
		Address: n.IP + "/" + strconv.Itoa(ones), MAC: n.MAC, Gateway: n.Gateway, Nameservers: n.Nameservers, Search: n.Search,
		APIServer: spec.apiServer(), MachinePrefix: n.MachinePrefix.String(),
		Guest: g.guest, Firecracker: spec.Inputs.FirecrackerSHA256,
		Disable: spec.Disable, Binaries: map[string]string{},
		Out: filepath.Join(dir, "naru.yaml"), NetworkConfig: filepath.Join(dir, "network-config"),
	}
	if c.hostNIC() {
		a.TimeServers = spec.TimeServers
		a.ExtraNIC = c.HostMAC() + "=" + HostNICPrefix
	}
	if c.ControlPlane {
		a.ServingAddresses = []string{"127.0.0.1"}
	}
	if spec.ha() {
		for i := 1; i <= spec.ControlPlanes; i++ {
			a.Members = append(a.Members, fmt.Sprintf("node-%d=%s", i, QEMUNetwork(i, spec.N).IP))
		}
	}
	for _, name := range spec.GaonBinaries {
		a.Binaries[name] = path.Join(OverrideDir, name)
	}
	a.AWSRegion, a.AWSTrustAnchor, a.AWSProfile, a.AWSRole = spec.AWSRegion, spec.AWSTrustAnchor, spec.AWSProfile, spec.AWSRole
	if spec.isHost(c.Index) {
		a.HostRegion, a.HostLabels = HostRegion, HostLabels
	}
	a.Registries = spec.Registries
	if a.Registries == nil {
		a.Registries = []string{"host=" + fixture.RegistryAddr + ",ca=" + filepath.Join(spec.Inputs.Dir, "registry", "ca.pem")}
	}
	if spec.GenFlags != nil {
		a.Extra = spec.GenFlags(c)
	}
	if err := spec.Naructl.GenNode(a); err != nil {
		return nil, nil, err
	}
	if spec.EditUserData != nil {
		if err := spec.EditUserData(c, a.Out); err != nil {
			return nil, nil, err
		}
	}
	for _, e := range spec.ExtraServices {
		svc := map[string]any{"name": e.Name, "binary": e.Binary}
		if len(e.Args) > 0 {
			svc["args"] = e.Args
		}
		if err := appendService(a.Out, svc); err != nil {
			return nil, nil, err
		}
	}
	if spec.isHost(c.Index) {
		if err := appendService(a.Out, map[string]any{
			"name": "naru88-mvmdlist", "binary": path.Join(fixture.GuestRoot, "bin/naru-mvmdlist"),
		}); err != nil {
			return nil, nil, err
		}
		if err := appendService(a.Out, map[string]any{
			"name": "naru88-mvmroute", "binary": path.Join(fixture.GuestRoot, "bin/naru-mvmroute"),
			"args": []string{"--listen", HostNICAddress + ":" + RoutePort, "--prefix", fmt.Sprintf("10.77.%d.0/24", c.Index)},
		}); err != nil {
			return nil, nil, err
		}
	}
	if spec.ha() && c.Index == spec.EtcdProbeOn {
		if err := addEtcdProbe(a.Out, spec, g.etcdClient, c.Index); err != nil {
			return nil, nil, err
		}
	}
	if userData, err = os.ReadFile(a.Out); err != nil {
		return nil, nil, err
	}
	networkConfig, err = os.ReadFile(a.NetworkConfig)
	return userData, networkConfig, err
}

// VirtualAddress is the address of the control plane of several members on the QEMU network: the
// one the holder takes (ADR 0024), which every agent dials and the API server's certificate names.
const VirtualAddress = "10.78.0.100"

// apiServer is the address the Nodes dial the control plane at: the virtual address of several
// members, or the control-plane Node's.
func (spec ClusterSpec) apiServer() string {
	if spec.ha() {
		return VirtualAddress
	}
	return QEMUNetwork(1, spec.N).IP
}

// nodeAPIFiles has naructl write the Node API's authority into the CA directory, so that the Nodes'
// user-data carries spec.api, and the client files of the host: an admin's, a reader's, and an
// admin's whose certificate is from another authority. The other authority lives only in its own
// directory, removed here.
func (g *gaonFiles) nodeAPIFiles(spec ClusterSpec) error {
	port, err := freePort()
	if err != nil {
		return fmt.Errorf("pick a host port for the Node API: %w", err)
	}
	g.apiPort = port
	addr := "127.0.0.1:" + strconv.Itoa(port)
	dir := filepath.Join(spec.Dir, "api-client")
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
		return err
	}
	api := &NodeAPI{Addr: addr, Admin: filepath.Join(dir, "admin.yaml"), Reader: filepath.Join(dir, "reader.yaml"), Foreign: filepath.Join(dir, "foreign.yaml")}
	if err := spec.Naructl.GenAPICA(g.caDir); err != nil {
		return err
	}
	for role, out := range map[string]string{"admin": api.Admin, "reader": api.Reader} {
		if err := spec.Naructl.GenAPIClient(g.caDir, role, addr, out); err != nil {
			return err
		}
	}
	other := filepath.Join(spec.Dir, "ca-foreign")
	defer func() { _ = os.RemoveAll(other) }()
	foreign := filepath.Join(dir, "foreign-own.yaml")
	if err := spec.Naructl.GenAPICA(other); err != nil {
		return err
	}
	if err := spec.Naructl.GenAPIClient(other, "admin", addr, foreign); err != nil {
		return err
	}
	if err := swapPair(api.Admin, foreign, api.Foreign); err != nil {
		return err
	}
	if err := os.Remove(foreign); err != nil {
		return err
	}
	g.api = api
	return nil
}

// swapPair writes to out the client file base with the certificate and key of the file from.
func swapPair(base, from, out string) error {
	read := func(p string) (map[string]any, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		return m, yaml.Unmarshal(b, &m)
	}
	f, err := read(base)
	if err != nil {
		return err
	}
	other, err := read(from)
	if err != nil {
		return err
	}
	f["certificate"], f["key"] = other["certificate"], other["key"]
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(out, b, 0o600)
}

// ProbeAlias is the address, in the Nodes' network and in no member's list, that naru-etcdprobe adds
// to its card and dials from.
const ProbeAlias = "10.78.0.50/24"

// addEtcdProbe appends naru-etcdprobe to the services of the user-data in file, the last to start.
// It dials from an address of the Node's own MicroVM prefix too, and with the pair in etcdClient.
func addEtcdProbe(file string, spec ClusterSpec, etcdClient string, index int) error {
	crt, err := os.ReadFile(filepath.Join(etcdClient, "tls.crt"))
	if err != nil {
		return err
	}
	key, err := os.ReadFile(filepath.Join(etcdClient, "tls.key"))
	if err != nil {
		return err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	var members []string
	for i := 1; i <= spec.ControlPlanes; i++ {
		members = append(members, QEMUNetwork(i, spec.N).IP)
	}
	var extra yaml.Node
	svc, err := yaml.Marshal(map[string]any{
		"name": "naru59-etcdprobe", "binary": path.Join(fixture.GuestRoot, "bin/naru-etcdprobe"),
		"args": []string{
			"-member", strings.Join(members, ","), "-vip", VirtualAddress, "-alias", ProbeAlias,
			"-machine-alias", fmt.Sprintf("10.77.%d.50/24", index),
			"-snapshot-cert-pem", string(crt), "-snapshot-key-pem", string(key),
		},
	})
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(svc, &extra); err != nil {
		return err
	}
	services := yamlField(yamlField(doc.Content[0], "spec"), "services")
	if services == nil {
		return errors.New("the user-data has no spec.services")
	}
	services.Content = append(services.Content, extra.Content[0])
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}

func yamlField(m *yaml.Node, key string) *yaml.Node {
	if m == nil {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// hostAPIFiles has naructl write the host API's authority into the CA directory, so that the hosts'
// user-data carries spec.host, and, for each host Node, the client files of the test host: a
// provisioner's, a reader's, and a provisioner's whose certificate is from another authority. The
// other authority lives only in its own directory, removed here.
func (g *gaonFiles) hostAPIFiles(spec ClusterSpec) error {
	if err := spec.Naructl.GenHostCA(g.caDir); err != nil {
		return err
	}
	other := filepath.Join(spec.Dir, "host-ca-foreign")
	defer func() { _ = os.RemoveAll(other) }()
	if err := spec.Naructl.GenHostCA(other); err != nil {
		return err
	}
	for _, i := range spec.hostNodes() {
		if err := g.hostAPIFilesOf(spec, i, other); err != nil {
			return err
		}
	}
	return nil
}

// hostAPIFilesOf makes the files of host Node i; the foreign authority is in other.
func (g *gaonFiles) hostAPIFilesOf(spec ClusterSpec, i int, other string) error {
	f := &hostFiles{}
	var err error
	if f.port, err = freePort(); err != nil {
		return fmt.Errorf("pick a host port for the host API: %w", err)
	}
	if f.routePort, err = freePort(); err != nil {
		return fmt.Errorf("pick a host port for the route: %w", err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(f.port)
	suffix := ""
	if i != 1 {
		suffix = fmt.Sprintf("-%d", i) // node-1's directory keeps its name
	}
	dir := filepath.Join(spec.Dir, "host-client"+suffix)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
		return err
	}
	h := &HostAPI{
		Node: fmt.Sprintf("node-%d", i), Index: i, Addr: addr,
		Provisioner: filepath.Join(dir, "provisioner.yaml"), Reader: filepath.Join(dir, "reader.yaml"), Foreign: filepath.Join(dir, "foreign.yaml"),
		Route: "127.0.0.1:" + strconv.Itoa(f.routePort),
	}
	for role, out := range map[string]string{"provisioner": h.Provisioner, "reader": h.Reader} {
		if err := spec.Naructl.GenHostClient(g.caDir, role, addr, out); err != nil {
			return err
		}
	}
	foreign := filepath.Join(dir, "foreign-own.yaml")
	if err := spec.Naructl.GenHostClient(other, "provisioner", addr, foreign); err != nil {
		return err
	}
	if err := swapPair(h.Provisioner, foreign, h.Foreign); err != nil {
		return err
	}
	if err := os.Remove(foreign); err != nil {
		return err
	}
	f.api = h
	g.hosts[i] = f
	g.hostList = append(g.hostList, h)
	return nil
}

// appendService appends svc to the services of the user-data in file, the last to start.
func appendService(file string, svc map[string]any) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	var extra yaml.Node
	enc, err := yaml.Marshal(svc)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(enc, &extra); err != nil {
		return err
	}
	services := yamlField(yamlField(doc.Content[0], "spec"), "services")
	if services == nil {
		return errors.New("the user-data has no spec.services")
	}
	services.Content = append(services.Content, extra.Content[0])
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}
