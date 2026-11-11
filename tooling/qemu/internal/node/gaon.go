package node

import (
	"fmt"
	"path"

	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

// Where gaon's pieces are on a Node, and the addresses it serves on (ADR 0012).
const (
	// GaonCertDir holds the certificate files init writes from spec.files; the services name them.
	GaonCertDir = "/run/gaon"
	// APIPort is the TCP port gaon-apiserver serves mutual TLS on.
	APIPort = "6443"
	// EtcdDir is etcd's data directory, on the data disk beside mvmd's state directory.
	EtcdDir = DataMount + "/etcd"
	// EtcdClientURL and EtcdPeerURL are loopback only: gaon-apiserver is the one client of etcd.
	EtcdClientURL = "http://127.0.0.1:2379"
	EtcdPeerURL   = "http://127.0.0.1:2380"

	// HostNICAddress is the address of the control-plane Node's second NIC, QEMU's user-mode
	// network, whose host forward carries the test host to the API server. The network is
	// 10.0.2.0/24 with the host side at 10.0.2.2, and the guest has no route off it.
	HostNICAddress = "10.0.2.15"
	hostNICNetmask = "255.255.255.0"
)

// Gaon makes a Node run gaon: every Node its Node Agent, and the Node that has spec.controlPlane
// also etcd, the API server, the scheduler and the controllers. It carries the certificates the
// tooling issued, which init writes under GaonCertDir.
type Gaon struct {
	// Certs holds every certificate of the cluster.
	Certs *pki.Cluster
	// APIServerIP is the address of the control-plane Node on the shared network, which every
	// agent dials and the serving certificate names.
	APIServerIP string
	// HostPort is the host port forwarded to the API server through the Node's second NIC, or 0
	// for a Node with no such NIC. Only the control-plane Node has one.
	HostPort int
}

// hostMAC is the address of the second NIC: the form of the first, in the next block of the
// locally administered range so that no two cards of a cluster share one.
func (c Config) hostMAC() string {
	return fmt.Sprintf("02:00:00:01:%02x:%02x", c.Index>>8, c.Index&0xff)
}

// hostNIC reports whether the Node has the second NIC.
func (c Config) hostNIC() bool { return c.Gaon != nil && c.Gaon.HostPort != 0 }

// APIRoot is the root of the resource API as the agents are given it.
func (g *Gaon) APIRoot() string {
	return "https://" + g.APIServerIP + ":" + APIPort + "/apis/compute.gaon.garam.sh/v1alpha1"
}

func gaonPath(name string) string { return path.Join(GaonCertDir, name) }

// files are the certificate files of the Node: the CA and the agent's pair on every Node, and, on
// the control-plane Node, the API server's pair and the scheduler's and the controllers' pairs.
// The keys are 0600 and the certificates 0644.
func (g *Gaon) files(c Config) []file {
	files := []file{
		{Path: gaonPath("ca.pem"), Mode: "0644", Content: string(g.Certs.CA)},
		{Path: gaonPath("agent.pem"), Mode: "0644", Content: string(g.Certs.Agents[c.Name].CertPEM)},
		{Path: gaonPath("agent.key"), Mode: "0600", Content: string(g.Certs.Agents[c.Name].KeyPEM)},
	}
	if !c.ControlPlane {
		return files
	}
	for _, p := range []struct {
		name string
		pair pki.Pair
	}{
		{"apiserver", g.Certs.APIServer},
		{"scheduler", g.Certs.Scheduler},
		{"controllers", g.Certs.Controllers},
	} {
		files = append(files,
			file{Path: gaonPath(p.name + ".pem"), Mode: "0644", Content: string(p.pair.CertPEM)},
			file{Path: gaonPath(p.name + ".key"), Mode: "0600", Content: string(p.pair.KeyPEM)},
		)
	}
	return files
}

// services are gaon's, in the order init starts them: etcd, then the API server that dials it, then
// the scheduler and the controllers that dial the API server, on the control-plane Node (init
// leaves them out elsewhere); the Node Agent, which dials mvmd's socket and the API server, last.
// Each dials what is before it and retries or is restarted by init, so the order is a courtesy and
// no service depends on it for correctness (docs/architecture/image.md).
func (g *Gaon) services(c Config) []service {
	client := func(name string) []string {
		return []string{"--tls-ca", gaonPath("ca.pem"), "--tls-cert", gaonPath(name + ".pem"), "--tls-key", gaonPath(name + ".key")}
	}
	return []service{
		{
			Name: "etcd", Binary: "/bin/etcd", ControlPlane: true,
			Args: []string{
				"--name", c.Name, "--data-dir", EtcdDir,
				"--listen-client-urls", EtcdClientURL, "--advertise-client-urls", EtcdClientURL,
				"--listen-peer-urls", EtcdPeerURL, "--initial-advertise-peer-urls", EtcdPeerURL,
				"--initial-cluster", c.Name + "=" + EtcdPeerURL,
			},
		},
		{
			Name: "gaon-apiserver", Binary: "/bin/gaon-apiserver", ControlPlane: true,
			Args: []string{
				"--listen", "0.0.0.0:" + APIPort, "--etcd-endpoints", EtcdClientURL,
				"--tls-client-ca", gaonPath("ca.pem"), "--tls-cert", gaonPath("apiserver.pem"), "--tls-key", gaonPath("apiserver.key"),
			},
		},
		{
			Name: "gaon-scheduler", Binary: "/bin/gaon-scheduler", ControlPlane: true,
			Args: append([]string{"--api", g.APIRoot()}, client("scheduler")...),
		},
		{
			Name: "gaon-controllers", Binary: "/bin/gaon-controllers", ControlPlane: true,
			Args: append([]string{"--api", g.APIRoot()}, client("controllers")...),
		},
		{
			Name: "gaon-node-agent", Binary: "/bin/gaon-node-agent",
			Args: append([]string{"--api-server", g.APIRoot(), "--node-name", c.Name, "--mvmd-socket", MvmdSocket}, client("agent")...),
		},
	}
}
