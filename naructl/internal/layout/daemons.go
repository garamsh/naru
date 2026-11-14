package layout

import (
	"errors"
	"fmt"
	"net/netip"
)

// spec.host.network, spec.signer, spec.orch and spec.metrics, which make init run the virtual
// networks' part of naru-hostd, naru-signer and naru-orchd
// (docs/architecture/adr/0050-the-image-carries-orch-and-the-signer-and-init-runs-them-as-separate-users-from-spec-orch-spec-signer-and-spec-metrics.md).
// naructl does not import init's config package, so these mirror its fields.
const (
	// OrchKeyFile is orch's private key, the one file of orch's directory that spec.files carries,
	// and OrchUID the user and group orch runs as and so owns it.
	OrchKeyFile        = "/run/naru-orchd/orch.key"
	OrchUID     uint32 = 6002
)

// HostNetwork is spec.host.network: what naru-hostd's virtual networks are kept from.
type HostNetwork struct {
	ManagementPrefix netip.Prefix
	// EgressInterface is empty to give the networks no egress, and TunnelPort 0 for 51820.
	EgressInterface string
	TunnelPort      int
}

// HostOrch is spec.host.orch: what naru-hostd enrolls with orch from. EnrollKey is written to a
// file for hostd alone and never printed.
type HostOrch struct {
	URL, CASHA256, EnrollKey string
}

// Signer is spec.signer: the signer this Node runs. Its bootstrap, the recovery key's public half
// (a JSON document) and the PEM certificate request of the first root identity, is both or neither.
// A Replica signer is made a replica of another instead: it makes its join key at its first start,
// and Join, once root has authorised that key, says whom to join.
type Signer struct {
	PeerListen        string
	PeerNames         []string
	RecoveryPublicKey string
	RootCSR           string
	Replica           bool
	Join              *SignerJoin
}

// SignerJoin is spec.signer.replica.join: the peer listener (host:port) of a signer that holds the
// authorities, the PEM of the orch authority its certificate chains to, and root's authorisation.
type SignerJoin struct {
	Peer, PeerCA, Authorisation string
}

// Orch is spec.orch: the orch member this Node runs, with its private key, which spec.files
// carries at OrchKeyFile for orch's user alone. The member's name is the Node's.
type Orch struct {
	PeerURL, ClientURL string
	// InitialCluster maps each member's name to its peer URL, this Node's among them.
	InitialCluster map[string]string
	// State is "new" or "existing"; empty is "new".
	State  string
	Listen string
	SANs   []string
	// Certificate is orch's certificate then the authority's, CA the authority's and
	// RevocationPublicKey the PKIX PEM of the revocation key: all PEM.
	Certificate, CA, RevocationPublicKey string
	// RecoveryPublicKey is the recovery key's public half, a JSON document; empty writes no snapshot.
	RecoveryPublicKey string
	// Key is the PEM of orch's private key. It is written to the file and never printed.
	Key string
	// Restore starts orchd waiting for root's snapshot (spec.orch.restore).
	Restore bool
}

type hostOrchSpec struct {
	URL       string `yaml:"url"`
	CASHA256  string `yaml:"caSHA256"`
	EnrollKey string `yaml:"enrollKey"`
}

type hostNetworkSpec struct {
	ManagementPrefix string `yaml:"managementPrefix"`
	EgressInterface  string `yaml:"egressInterface,omitempty"`
	TunnelPort       int    `yaml:"tunnelPort,omitempty"`
}

type signerSpec struct {
	PeerListen string           `yaml:"peerListen,omitempty"`
	PeerNames  []string         `yaml:"peerNames,omitempty"`
	Bootstrap  *signerBootstrap `yaml:"bootstrap,omitempty"`
	Replica    *signerReplica   `yaml:"replica,omitempty"`
}

type signerReplica struct {
	Join *signerJoin `yaml:"join,omitempty"`
}

type signerJoin struct {
	Peer          string `yaml:"peer"`
	PeerCA        string `yaml:"peerCA"`
	Authorisation string `yaml:"authorisation"`
}

type signerBootstrap struct {
	RecoveryPublicKey string `yaml:"recoveryPublicKey"`
	RootCSR           string `yaml:"rootCSR"`
}

type orchSpec struct {
	PeerURL             string            `yaml:"peerURL"`
	ClientURL           string            `yaml:"clientURL"`
	InitialCluster      map[string]string `yaml:"initialCluster"`
	InitialClusterState string            `yaml:"initialClusterState,omitempty"`
	Listen              string            `yaml:"listen,omitempty"`
	SANs                []string          `yaml:"sans,omitempty"`
	Certificate         string            `yaml:"certificate"`
	CA                  string            `yaml:"ca"`
	RevocationPublicKey string            `yaml:"revocationPublicKey"`
	RecoveryPublicKey   string            `yaml:"recoveryPublicKey,omitempty"`
	Restore             bool              `yaml:"restore,omitempty"`
}

type metricsSpec struct {
	Listen string `yaml:"listen"`
}

// fileOwner is a file's owner, numeric.
type fileOwner struct {
	UID uint32 `yaml:"uid"`
	GID uint32 `yaml:"gid"`
}

// checkDaemons reports the first thing init would refuse in the sections above, which would stop
// the Node booting or leave it without what orch needs.
func (n Node) checkDaemons() error {
	if h := n.HostNetwork; h != nil {
		if n.Host == nil {
			return errors.New("the host network needs the host API: give the host authority and a region")
		}
		if !h.ManagementPrefix.IsValid() || h.ManagementPrefix != h.ManagementPrefix.Masked() {
			return fmt.Errorf("the management prefix %s is not a prefix with no host bits", h.ManagementPrefix)
		}
		if h.TunnelPort < 0 || h.TunnelPort > 65535 {
			return fmt.Errorf("the tunnel port %d is not a port", h.TunnelPort)
		}
	}
	if s := n.Signer; s != nil {
		if (s.RecoveryPublicKey == "") != (s.RootCSR == "") {
			return errors.New("the signer's bootstrap needs both the recovery public key and the root request, or neither")
		}
		if s.Replica && s.RootCSR != "" {
			return errors.New("a signer is initialised or made a replica, not both")
		}
		if s.Join != nil && !s.Replica {
			return errors.New("the signer joins as a replica: give --signer-replica")
		}
		if j := s.Join; j != nil && (j.Peer == "" || j.PeerCA == "" || j.Authorisation == "") {
			return errors.New("joining needs the peer, the PEM of the orch authority and root's authorisation")
		}
		if (s.PeerListen == "") != (len(s.PeerNames) == 0) {
			return errors.New("the signer's peer listener needs both an address and the names of its certificate, or neither")
		}
	}
	if o := n.Orch; o != nil {
		if n.Signer == nil {
			return errors.New("orch asks the signer on this Node for every identity: run the signer here")
		}
		if o.PeerURL == "" || o.ClientURL == "" || len(o.InitialCluster) == 0 {
			return errors.New("orch needs its peer URL, its client URL and the members of its cluster")
		}
		if o.InitialCluster["naru-"+n.Name] != o.PeerURL {
			return fmt.Errorf("the initial cluster holds %q for this Node, not its peer URL %q", o.InitialCluster["naru-"+n.Name], o.PeerURL)
		}
		if o.Certificate == "" || o.CA == "" || o.RevocationPublicKey == "" || o.Key == "" {
			return errors.New("orch needs its certificate, the authority, the revocation public key and its key")
		}
		if s := o.State; s != "" && s != "new" && s != "existing" {
			return fmt.Errorf(`the initial cluster state is "new" or "existing", not %q`, s)
		}
	}
	if h := n.HostOrch; h != nil {
		if n.Host == nil {
			return errors.New("enrolling hostd with orch needs the host API: give the host authority and a region")
		}
		if h.URL == "" || h.CASHA256 == "" || h.EnrollKey == "" {
			return errors.New("hostd's enrollment needs orch's URL, the SHA-256 of its authority and the enrollment key")
		}
	}
	if n.MetricsListen != "" {
		if a, err := netip.ParseAddr(n.MetricsListen); err != nil || a.Zone() != "" {
			return fmt.Errorf("the metrics address %q is not an IP address", n.MetricsListen)
		}
	}
	return nil
}

// daemonSpecs sets the sections and the orch key into the configuration.
func (n Node) daemonSpecs(c *nodeConfig) {
	if h := n.HostNetwork; h != nil && c.Spec.Host != nil {
		c.Spec.Host.Network = &hostNetworkSpec{ManagementPrefix: h.ManagementPrefix.String(), EgressInterface: h.EgressInterface, TunnelPort: h.TunnelPort}
	}
	if h := n.HostOrch; h != nil && c.Spec.Host != nil {
		c.Spec.Host.Orch = &hostOrchSpec{URL: h.URL, CASHA256: h.CASHA256, EnrollKey: h.EnrollKey}
	}
	if s := n.Signer; s != nil {
		c.Spec.Signer = &signerSpec{PeerListen: s.PeerListen, PeerNames: s.PeerNames}
		if s.Replica {
			c.Spec.Signer.Replica = &signerReplica{}
			if j := s.Join; j != nil {
				c.Spec.Signer.Replica.Join = &signerJoin{Peer: j.Peer, PeerCA: j.PeerCA, Authorisation: j.Authorisation}
			}
		}
		if s.RootCSR != "" {
			c.Spec.Signer.Bootstrap = &signerBootstrap{RecoveryPublicKey: s.RecoveryPublicKey, RootCSR: s.RootCSR}
		}
	}
	if o := n.Orch; o != nil {
		c.Spec.Orch = &orchSpec{
			PeerURL: o.PeerURL, ClientURL: o.ClientURL, InitialCluster: o.InitialCluster, InitialClusterState: o.State,
			Listen: o.Listen, SANs: o.SANs, Certificate: o.Certificate, CA: o.CA,
			RevocationPublicKey: o.RevocationPublicKey, RecoveryPublicKey: o.RecoveryPublicKey, Restore: o.Restore,
		}
		c.Spec.Files = append(c.Spec.Files, file{Path: OrchKeyFile, Mode: "0600", Content: o.Key, Owner: &fileOwner{UID: OrchUID, GID: OrchUID}})
	}
	if n.MetricsListen != "" {
		c.Spec.Metrics = &metricsSpec{Listen: n.MetricsListen}
	}
}
