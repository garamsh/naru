package command

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/garamsh/naru/naructl/internal/layout"
)

// daemonFlags are the values of gen node's flags for spec.host.network, spec.signer, spec.orch
// and spec.metrics.
type daemonFlags struct {
	hostMgmt, hostEgress                                                   string
	hostTunnelPort                                                         int
	hostOrchURL, hostOrchCA, hostOrchKey                                   string
	signerReplica                                                          bool
	signerJoinPeer, signerJoinPeerCA, signerJoinAuthz                      string
	runSigner                                                              bool
	signerPeer, signerRecovery, signerRootCSR                              string
	signerPeerNames                                                        list
	orchPeerURL, orchClientURL, orchState, orchListen                      string
	orchCert, orchCA, orchRevocation, orchRecovery, orchKey, metricsListen string
	orchHostCA, orchJoin                                                   string
	orchRestore                                                            bool
	orchCluster, orchSANs, extensions                                      list
}

// genNodeDaemons sets what the flags say of those sections on the Node, reading the files they
// name. The orch key is read into the Node and never printed.
func genNodeDaemons(n *layout.Node, f daemonFlags) error {
	for _, e := range f.extensions {
		name, digest, ok := strings.Cut(e, "=")
		if !ok || name == "" || digest == "" {
			return fmt.Errorf("--extension %q is not <name>=<SHA-256 of the image>", e)
		}
		n.Extensions = append(n.Extensions, layout.Extension{Name: name, SHA256: digest})
	}
	if f.hostMgmt != "" {
		p, err := netip.ParsePrefix(f.hostMgmt)
		if err != nil {
			return fmt.Errorf("--host-management-prefix: %w", err)
		}
		n.HostNetwork = &layout.HostNetwork{ManagementPrefix: p, EgressInterface: f.hostEgress, TunnelPort: f.hostTunnelPort}
	}
	if f.hostOrchURL != "" {
		if f.hostOrchKey == "" {
			return errors.New("--host-orch-url needs --host-orch-enroll-key")
		}
		key, err := readText("--host-orch-enroll-key", f.hostOrchKey)
		if err != nil {
			return err
		}
		n.HostOrch = &layout.HostOrch{URL: f.hostOrchURL, CASHA256: f.hostOrchCA, EnrollKey: key}
	}
	if f.runSigner {
		s := &layout.Signer{PeerListen: f.signerPeer, PeerNames: f.signerPeerNames}
		var err error
		if f.signerRecovery != "" {
			if s.RecoveryPublicKey, err = readText("--signer-recovery-public", f.signerRecovery); err != nil {
				return err
			}
		}
		if f.signerRootCSR != "" {
			if s.RootCSR, err = readText("--signer-root-csr", f.signerRootCSR); err != nil {
				return err
			}
		}
		if f.signerReplica {
			s.Replica = true
			if f.signerJoinPeer != "" || f.signerJoinPeerCA != "" || f.signerJoinAuthz != "" {
				s.Join = &layout.SignerJoin{Peer: f.signerJoinPeer}
				if s.Join.PeerCA, err = readText("--signer-join-peer-ca", f.signerJoinPeerCA); err != nil {
					return err
				}
				if s.Join.Authorisation, err = readText("--signer-join-authorisation", f.signerJoinAuthz); err != nil {
					return err
				}
			}
		}
		n.Signer = s
	}
	if f.orchPeerURL != "" {
		o := &layout.Orch{PeerURL: f.orchPeerURL, ClientURL: f.orchClientURL, State: f.orchState, Join: f.orchJoin, Listen: f.orchListen, SANs: f.orchSANs, InitialCluster: map[string]string{}}
		for _, m := range f.orchCluster {
			name, url, ok := strings.Cut(m, "=")
			if !ok || name == "" || url == "" {
				return fmt.Errorf("--orch-member %q is not <name>=<peer URL>", m)
			}
			o.InitialCluster[name] = url
		}
		o.Restore = f.orchRestore
		for _, r := range []struct {
			flag, path string
			into       *string
		}{
			{"orch-certificate", f.orchCert, &o.Certificate}, {"orch-ca", f.orchCA, &o.CA}, {"orch-host-ca", f.orchHostCA, &o.HostCA},
			{"orch-revocation-public-key", f.orchRevocation, &o.RevocationPublicKey},
			{"orch-recovery-public", f.orchRecovery, &o.RecoveryPublicKey}, {"orch-key", f.orchKey, &o.Key},
		} {
			if r.path == "" {
				continue
			}
			var err error
			if *r.into, err = readText("--"+r.flag, r.path); err != nil {
				return err
			}
		}
		n.Orch = o
	}
	n.MetricsListen = f.metricsListen
	return nil
}

func readText(flag, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flag, err)
	}
	return string(b), nil
}
