package layout

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// The ports etcd serves on, and the files its certificates are.
const (
	EtcdClientPort = "2379"
	EtcdPeerPort   = "2380"
)

// etcd's pairs, besides the API server's: the one etcd serves clients and peers with, the one it
// presents when it dials a peer, and the one the API server presents when it dials etcd. init issues
// them from spec.pki, or they are files, beside the others.
var (
	etcdServer  = pairSpec{"etcd-server", "etcd"}
	etcdPeer    = pairSpec{"etcd-peer", "etcd-peer"}
	apiserverTo = pairSpec{"apiserver-etcd", "gaon-apiserver-etcd"}
)

// Member is a control-plane Node, one of the three members of etcd's static cluster.
type Member struct {
	Name    string
	Address netip.Addr
}

// members are the Node's control-plane Nodes in priority order, the Node's own among them: Members,
// or the Node alone.
func (n Node) members() []Member {
	if len(n.Members) == 0 {
		return []Member{{Name: n.Name, Address: n.Address.Addr()}}
	}
	return n.Members
}

// HA reports whether the control plane has several members: etcd listens on the Node's address,
// the virtual address is held, and the rules close etcd's ports to the members' addresses only.
func (n Node) HA() bool { return len(n.Members) > 1 }

// checkMembers refuses a list of members the Node cannot be one of.
func (n Node) checkMembers() error {
	if len(n.Members) == 0 {
		return nil
	}
	if !n.Mode.ControlPlane() {
		return errors.New("only a control-plane Node has members")
	}
	self, seen := false, map[string]bool{}
	for _, m := range n.Members {
		switch {
		case !dnsLabel.MatchString(m.Name):
			return fmt.Errorf("the member name %q is not a DNS label", m.Name)
		case !m.Address.IsValid() || !m.Address.Is4():
			return fmt.Errorf("the address of member %s is not IPv4", m.Name)
		case seen[m.Name] || seen[m.Address.String()]:
			return fmt.Errorf("member %s repeats a name or an address", m.Name)
		case m.Address == n.APIServer:
			return fmt.Errorf("member %s has the address of the virtual address; it is no member's own", m.Name)
		}
		seen[m.Name], seen[m.Address.String()] = true, true
		self = self || (m.Name == n.Name && m.Address == n.Address.Addr())
	}
	if !self {
		return fmt.Errorf("the Node %s at %s is not among its members", n.Name, n.Address.Addr())
	}
	if n.HA() && !n.APIServer.IsValid() {
		return errors.New("a control plane of several members needs the virtual address")
	}
	return nil
}

func etcdURL(a netip.Addr, port string) string {
	return "https://" + net.JoinHostPort(a.String(), port)
}

// listenAddrs are the addresses etcd listens on: the loopback, and in HA the Node's own, which the
// other members dial.
func (n Node) listenAddrs() []netip.Addr {
	lo := netip.MustParseAddr("127.0.0.1")
	if !n.HA() {
		return []netip.Addr{lo}
	}
	return []netip.Addr{n.Address.Addr(), lo}
}

// etcdServerIPs are the addresses etcd's serving certificate names when it is a file: the ones it
// listens on.
func (n Node) etcdServerIPs() []net.IP {
	var ips []net.IP
	for _, a := range n.listenAddrs() {
		ips = append(ips, net.IP(a.AsSlice()))
	}
	return ips
}

// etcdArgs are etcd's: its name, data directory and static cluster, and its TLS on both ports.
func (n Node) etcdArgs() []string {
	var listen, initial []string
	for _, a := range n.listenAddrs() {
		listen = append(listen, etcdURL(a, EtcdClientPort))
	}
	own := n.listenAddrs()[0]
	for _, m := range n.members() {
		addr := m.Address
		if !n.HA() {
			addr = own // a lone member is dialled on the loopback, which its certificate names
		}
		initial = append(initial, m.Name+"="+etcdURL(addr, EtcdPeerPort))
	}
	return []string{
		"--name", n.Name, "--data-dir", EtcdDir,
		"--listen-client-urls", strings.Join(listen, ","), "--advertise-client-urls", etcdURL(own, EtcdClientPort),
		"--listen-peer-urls", etcdURL(own, EtcdPeerPort), "--initial-advertise-peer-urls", etcdURL(own, EtcdPeerPort),
		"--initial-cluster", strings.Join(initial, ","),
		"--cert-file", gaonPath(etcdServer.name + ".pem"), "--key-file", gaonPath(etcdServer.name + ".key"),
		"--client-cert-auth", "--trusted-ca-file", gaonPath("ca.pem"),
		"--peer-cert-file", gaonPath(etcdServer.name + ".pem"), "--peer-key-file", gaonPath(etcdServer.name + ".key"),
		"--peer-client-cert-file", gaonPath(etcdPeer.name + ".pem"), "--peer-client-key-file", gaonPath(etcdPeer.name + ".key"),
		"--peer-client-cert-auth", "--peer-trusted-ca-file", gaonPath("ca.pem"),
		// The gateway serves etcd's JSON API, which nothing here uses, and dials the client port with the serving pair, whose certificate has no clientAuth: its handshake fails and etcd logs it.
		"--enable-grpc-gateway=false",
	}
}

// etcdEndpoints are the client URLs the API server dials: every member's, in the order of Members,
// the same list on every Node as gaon's deployment.md has it; a control plane of one Node is
// dialled on the loopback, which its certificate names.
func (n Node) etcdEndpoints() string {
	if !n.HA() {
		return etcdURL(n.listenAddrs()[0], EtcdClientPort)
	}
	var urls []string
	for _, m := range n.Members {
		urls = append(urls, etcdURL(m.Address, EtcdClientPort))
	}
	return strings.Join(urls, ",")
}

// haServices are the two services a control-plane Node of an HA control plane runs besides the
// others: the rules that close etcd's ports, which start before etcd, and the holder of the virtual
// address, which starts after the API server whose answer it watches.
func (n Node) haServices() (fence, vip service) {
	var members, allowed []string
	for _, m := range n.Members {
		members = append(members, m.Address.String())
		allowed = append(allowed, m.Address.String())
	}
	allowed = append(allowed, n.APIServer.String())
	fence = service{
		Name: "fence", ControlPlane: true, Binary: n.binary("fence", "/bin/naru-fence"),
		Args: []string{"-allow", strings.Join(slices.Compact(allowed), ","), "-allow-machines", n.MachinePrefix.Masked().String()},
	}
	vip = service{
		Name: "vip", ControlPlane: true, Binary: n.binary("vip", "/bin/naru-vip"),
		Args: []string{
			"-address", netip.PrefixFrom(n.APIServer, n.Address.Bits()).String(), "-interface", "eth0",
			"-self", n.Address.Addr().String(), "-member", strings.Join(members, ","), "-port", APIPort,
			"-etcd-ca-file", gaonPath("ca.pem"), "-etcd-cert-file", gaonPath(apiserverTo.name + ".pem"), "-etcd-key-file", gaonPath(apiserverTo.name + ".key"),
		},
	}
	return fence, vip
}
