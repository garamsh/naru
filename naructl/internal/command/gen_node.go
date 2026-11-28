package command

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/layout/pki"
)

// nodeFlags are the values of gen node's flags. The sections of the Node that are daemons have
// theirs in daemonFlags.
type nodeFlags struct {
	daemonFlags
	n layout.Node

	caDir, address, gateway, machinePrefix, extraNIC, out, networkConfig                             string
	awsRegion, awsTrustAnchor, awsProfile, awsRole, hostRegion, hostClientCA, hostRevocation         string
	nameservers, serving, timeServers, peers, registries, storage, noMasquerade, hostLabels, disable list
}

// genNode is gen node: it writes the user data (and with --network-config the NoCloud network
// config) of one Node.
func genNode(fs *pflag.FlagSet) runner {
	var f nodeFlags
	f.declare(fs)
	return func(_ []string, stdout, _ io.Writer) error {
		if err := f.check(fs); err != nil {
			return err
		}
		if err := f.build(); err != nil {
			return err
		}
		return f.write(stdout)
	}
}

func (f *nodeFlags) declare(fs *pflag.FlagSet) {
	fs.StringVar(&f.caDir, "ca-dir", "", "the authorities' directory: the Node serves the Node API from api/ when gen api-ca wrote it, and the host API from host/ with --host-region (the keys are read here and never carried)")
	fs.StringVar(&f.n.Name, "name", "", "the Node's name, such as node-1")
	fs.StringVar(&f.address, "address", "", "the Node's address and prefix, such as 192.168.0.40/24")
	fs.StringVar(&f.n.MAC, "mac", "", "the MAC address of the Node's card (with --network-config)")
	fs.StringVar(&f.gateway, "gateway", "", "the default gateway (with --network-config)")
	fs.Var(&f.nameservers, "nameserver", "a nameserver, repeatable (with --network-config)")
	fs.StringVar(&f.n.Search, "search", "", "the search domain (with --network-config)")
	fs.StringVar(&f.extraNIC, "extra-nic", "", "a second card, <mac>=<cidr>, with no gateway (with --network-config)")
	fs.Var(&f.serving, "serving-address", "another address the Node API's and the host API's certificates name, repeatable")
	fs.Var(&f.timeServers, "time-server", "an SNTP server init sets the clock from, host or host:port, repeatable and asked in order (none: the clock is not set)")
	fs.Var(&f.disable, "disable", "a service to leave out: registry, the only one a Node runs that it can do without, for Nodes that pull from elsewhere")
	fs.Var(&f.registries, "registry", "a registry mvmd pulls from, host=<host[:port]> then any of ca=<PEM file>, username=<name> with password-file=<file>, or helper=<name> (a username with a helper is refused), comma-separated, repeatable")
	fs.StringVar(&f.awsRegion, "aws-region", "", "the AWS region of the IAM Roles Anywhere profile the credential helpers use (with the other --aws flags: all four or none)")
	fs.StringVar(&f.awsTrustAnchor, "aws-trust-anchor", "", "the ARN of the IAM Roles Anywhere trust anchor")
	fs.StringVar(&f.awsProfile, "aws-profile", "", "the ARN of the IAM Roles Anywhere profile")
	fs.StringVar(&f.awsRole, "aws-role", "", "the ARN of the IAM role; the certificate and key are naru-hostd's, so the --aws flags need --host-region")
	fs.StringVar(&f.hostRegion, "host-region", "", "the Node serves the host API (naru-hostd) and reports this region, a DNS label; needs the host authority, gen host-ca")
	fs.Var(&f.hostLabels, "host-label", "a label of the host, <key>=<value>, repeatable (with --host-region)")
	fs.StringVar(&f.hostMgmt, "host-management-prefix", "", "spec.host.network: the prefix of the management plane, which no virtual network may reach, such as 192.168.0.0/24 (with --host-region)")
	fs.StringVar(&f.hostEgress, "host-egress-interface", "", "spec.host.network: the interface the virtual networks' egress leaves by (with --host-management-prefix)")
	fs.IntVar(&f.hostTunnelPort, "host-tunnel-port", 0, "spec.host.network: the UDP port of the WireGuard tunnel (with --host-management-prefix; default 51820)")
	fs.BoolVar(&f.runSigner, "signer", false, "the Node runs naru-signer (spec.signer)")
	fs.StringVar(&f.signerPeer, "signer-peer-listen", "", "spec.signer: the address of the peer listener other signers replicate from (with --signer-peer-name)")
	fs.Var(&f.signerPeerNames, "signer-peer-name", "spec.signer: a name or address of the peer listener's certificate, repeatable")
	fs.StringVar(&f.hostOrchURL, "host-orch-url", "", "spec.host.orch: the https URL of orch's API, which naru-hostd enrolls with (with --host-region)")
	fs.StringVar(&f.hostOrchCA, "host-orch-ca-sha256", "", "spec.host.orch: the hex SHA-256 of the public key of the authority orch's certificate chains to (naructl orch ca prints it)")
	fs.StringVar(&f.hostOrchKey, "host-orch-enroll-key", "", "spec.host.orch: the file holding the enrollment key, which the user data carries 0600 for naru-hostd alone")
	fs.BoolVar(&f.signerReplica, "signer-replica", false, "spec.signer.replica: the signer is a replica of another and makes its join key at its first start (excludes --signer-root-csr)")
	fs.StringVar(&f.signerJoinPeer, "signer-join-peer", "", "spec.signer.replica.join: host:port of the peer listener of a signer that holds the authorities (with --signer-replica)")
	fs.StringVar(&f.signerJoinPeerCA, "signer-join-peer-ca", "", "spec.signer.replica.join: the file holding the PEM of the orch authority the peer's certificate chains to")
	fs.StringVar(&f.signerJoinAuthz, "signer-join-authorisation", "", "spec.signer.replica.join: the file holding root's authorisation, which naructl signer authorise-replica wrote")
	fs.StringVar(&f.signerRecovery, "signer-recovery-public", "", "spec.signer.bootstrap: the file holding the recovery key's public half (with --signer-root-csr)")
	fs.StringVar(&f.signerRootCSR, "signer-root-csr", "", "spec.signer.bootstrap: the file holding the PEM certificate request of the first root identity")
	fs.StringVar(&f.orchPeerURL, "orch-peer-url", "", "spec.orch: the https URL etcd listens on for its peers; giving it makes the Node run naru-orchd, which needs the other --orch flags and --signer")
	fs.StringVar(&f.orchClientURL, "orch-client-url", "", "spec.orch: the https URL etcd listens on for clients")
	fs.Var(&f.orchCluster, "orch-member", "spec.orch: <name>=<peer URL>, a member of orch's cluster, this Node among them, repeatable")
	fs.StringVar(&f.orchState, "orch-state", "", "spec.orch: new, or existing for a member that joins a cluster (default new)")
	fs.StringVar(&f.orchListen, "orch-listen", "", "spec.orch: the address of orch's HTTPS API (default 0.0.0.0:7500)")
	fs.StringVar(&f.orchMachineListen, "orch-machine-api-listen", "", "spec.orch: the address of the Machine API (default 0.0.0.0:7600), served only with --orch-machine-zone")
	fs.StringVar(&f.orchMachineZone, "orch-machine-zone", "", "spec.orch: the zone of Machines' stable names, m-<id>.<zone>, such as garam.run; without it orch serves no Machine API")
	fs.Var(&f.orchSANs, "orch-san", "spec.orch: a name or address of orch's certificate, repeatable")
	fs.StringVar(&f.orchCert, "orch-certificate", "", "spec.orch: the file holding orch's certificate then the authority's, PEM")
	fs.StringVar(&f.orchCA, "orch-ca", "", "spec.orch: the file holding the authority's certificate, PEM")
	fs.StringVar(&f.orchRevocation, "orch-revocation-public-key", "", "spec.orch: the file holding the revocation public key, PKIX PEM, which the signer's init printed")
	fs.StringVar(&f.orchHostCA, "orch-host-ca", "", "spec.orch: the file holding the PEM of the authority the hosts' API certificates chain to, the host authority; without it orch trusts --orch-ca for them")
	fs.StringVar(&f.hostClientCA, "host-client-ca", "", "spec.host: the file holding the PEM of the authority orch's certificate chains to, which naru-hostd trusts for its clients beside the host authority (with --host-region)")
	fs.StringVar(&f.hostRevocation, "host-revocation-public-key", "", "spec.host: the file holding the revocation public key, PKIX PEM, which the signer's init printed: naru-hostd checks its clients against the signed list, read from --host-orch-url (with --host-region)")
	fs.StringVar(&f.orchRecovery, "orch-recovery-public", "", "spec.orch: the file holding the recovery key's public half; without it orch writes no snapshot")
	fs.StringVar(&f.orchJoin, "orch-join", "", "spec.orch.join: the client URL, https://<host>:<port>, of a running member, which this member joins; it takes the initial cluster from that cluster, so no --orch-member, and excludes --orch-restore")
	fs.BoolVar(&f.orchRestore, "orch-restore", false, "spec.orch.restore: orchd starts empty and waits for root's etcd snapshot (PUT /v1/restore)")
	fs.StringVar(&f.orchKey, "orch-key", "", "spec.orch: the file holding orch's private key, which the user data carries 0600 for orch's user alone")
	fs.Var(&f.extensions, "extension", "spec.extensions: <name> or <name>=<SHA-256 of the image>, a system extension init mounts before any service starts, repeatable; <name> alone takes the stored image of that name built for the running naru version, which an upgrade bundle carries, and the form with a SHA-256 pins one image; the signer and orch need naru-orch")
	fs.StringVar(&f.metricsListen, "metrics-listen", "", "spec.metrics: the IP address init and the daemons serve /metrics on, each on its own port")
	fs.StringVar(&f.machinePrefix, "machine-prefix", "", "the IPv4 prefix mvmd gives its Machines, such as 10.200.1.0/24")
	fs.Var(&f.peers, "peer", "another Node on the Node's network, <address>=<machine-prefix> such as 192.168.0.61=10.200.2.0/24, repeatable: the Node routes to the prefix via the address, and its Machines keep their source address to it")
	fs.Var(&f.noMasquerade, "no-masquerade", "an IPv4 prefix in canonical form, repeatable: a Machine's traffic to it keeps its source address (mvmd's network.no_masquerade) and the Node gets no route to it, such as the Machine prefixes of the hosts a WireGuard tunnel reaches")
	fs.Var(&f.storage, "storage", "<name>=<label, /dev/path or serial:NUMBER>, an extra disk the Node offers as XFS, found by its partition or file system label, by its device when the value starts with /dev/, or by its disk serial number when it starts with serial: (a blank disk has no label, and the kernel's device names change between boots, so a blank disk is best named by serial; init formats it at the first boot and finds it by the XFS label after), repeatable: the first holds mvmd's disks, snapshots and Machine run directories, on XFS with reflinks, and the others are mounted under /var/lib/naru/storage")
	fs.StringVar(&f.n.FirecrackerSHA256, "firecracker-sha256", "", "the SHA-256 of the Firecracker binary the image carries")
	fs.StringVar(&f.out, "out", "", "the user-data file to write (0600: it carries keys; a file already there is replaced)")
	fs.StringVar(&f.networkConfig, "network-config", "", "also write the NoCloud network-config to this file")
}

// check refuses what the flags given say nothing of, or say twice.
func (f *nodeFlags) check(fs *pflag.FlagSet) error {
	if err := require(fs, "ca-dir", "name", "address", "machine-prefix", "firecracker-sha256", "out"); err != nil {
		return err
	}
	if err := awsTogether(fs); err != nil {
		return err
	}
	for _, g := range [][]string{
		{"network-config", "mac", "gateway", "nameserver", "search", "extra-nic"},
		{"host-region", "host-label", "host-client-ca", "host-revocation-public-key"},
		{"host-management-prefix", "host-egress-interface", "host-tunnel-port"},
		{"host-orch-url", "host-orch-ca-sha256", "host-orch-enroll-key"},
		{"signer-replica", "signer-join-peer", "signer-join-peer-ca", "signer-join-authorisation"},
		{"signer", "signer-replica", "signer-peer-listen", "signer-peer-name", "signer-recovery-public", "signer-root-csr"},
		{"orch-peer-url", "orch-client-url", "orch-member", "orch-state", "orch-listen", "orch-machine-api-listen", "orch-machine-zone", "orch-san", "orch-certificate", "orch-ca", "orch-revocation-public-key", "orch-recovery-public", "orch-key", "orch-restore", "orch-join", "orch-host-ca"},
	} {
		if err := needs(fs, g[0], g[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// build fills the Node from the flags, reading the authorities and files they name.
func (f *nodeFlags) build() error {
	n := &f.n
	if err := genNodeDaemons(n, f.daemonFlags); err != nil {
		return err
	}
	if err := f.authorities(); err != nil {
		return err
	}
	if err := f.network(); err != nil {
		return err
	}
	if err := f.services(); err != nil {
		return err
	}
	return nil
}

// authorities loads the Node API and host authorities, which the Node serves with.
func (f *nodeFlags) authorities() error {
	n := &f.n
	var err error
	if _, err = os.Stat(filepath.Join(f.caDir, pki.APIDir)); err == nil {
		if n.API, err = pki.Load(filepath.Join(f.caDir, pki.APIDir)); err != nil {
			return fmt.Errorf("the Node API authority: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if f.hostRegion != "" {
		if n.Host, err = loadHost(f.caDir, f.hostRegion, f.hostLabels); err != nil {
			return err
		}
		if f.hostClientCA != "" {
			if n.Host.ClientCA, err = readText("--host-client-ca", f.hostClientCA); err != nil {
				return err
			}
		}
		if f.hostRevocation != "" {
			if n.Host.RevocationPublicKey, err = readText("--host-revocation-public-key", f.hostRevocation); err != nil {
				return err
			}
		}
	}
	return nil
}

// network reads the addresses, the prefixes and the second card.
func (f *nodeFlags) network() error {
	n := &f.n
	var err error
	if n.Address, err = netip.ParsePrefix(f.address); err != nil {
		return fmt.Errorf("--address: %w", err)
	}
	if n.MachinePrefix, err = netip.ParsePrefix(f.machinePrefix); err != nil {
		return fmt.Errorf("--machine-prefix: %w", err)
	}
	if f.gateway != "" {
		if n.Gateway, err = netip.ParseAddr(f.gateway); err != nil {
			return fmt.Errorf("--gateway: %w", err)
		}
	}
	for _, s := range f.nameservers {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("--nameserver: %w", err)
		}
		n.Nameservers = append(n.Nameservers, a)
	}
	for _, s := range f.serving {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("--serving-address: %w", err)
		}
		n.ServingAddresses = append(n.ServingAddresses, a)
	}
	for _, s := range f.peers {
		addr, prefix, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("--peer %q is not <address>=<machine-prefix>", s)
		}
		a, err := netip.ParseAddr(addr)
		if err != nil {
			return fmt.Errorf("--peer: %w", err)
		}
		p, err := netip.ParsePrefix(prefix)
		if err != nil {
			return fmt.Errorf("--peer: %w", err)
		}
		n.Peers = append(n.Peers, layout.Peer{Address: a, MachinePrefix: p})
	}
	for _, s := range f.noMasquerade {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return fmt.Errorf("--no-masquerade %q is not an IPv4 prefix in canonical form", s)
		}
		n.NoMasquerade = append(n.NoMasquerade, p)
	}
	if f.extraNIC != "" {
		mac, cidr, ok := strings.Cut(f.extraNIC, "=")
		if !ok {
			return fmt.Errorf("--extra-nic %q is not <mac>=<cidr>", f.extraNIC)
		}
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("--extra-nic: %w", err)
		}
		n.ExtraNIC = &layout.NIC{MAC: mac, Address: p}
	}
	return nil
}

// services reads the disks, the time servers, the registries and the AWS profile.
func (f *nodeFlags) services() error {
	n := &f.n
	for _, s := range f.storage {
		d, err := parseStorage(s)
		if err != nil {
			return err
		}
		n.Storage = append(n.Storage, d)
	}
	n.TimeServers = f.timeServers
	for _, s := range f.disable {
		if s != "registry" {
			return fmt.Errorf("--disable %q: registry is the only service gen node leaves out", s)
		}
		n.NoRegistry = true
	}
	for _, r := range f.registries {
		reg, err := parseRegistry(r)
		if err != nil {
			return fmt.Errorf("--registry: %w", err)
		}
		n.Registries = append(n.Registries, reg)
	}
	if f.awsRegion != "" {
		n.AWS = &layout.AWS{Region: f.awsRegion, TrustAnchorArn: f.awsTrustAnchor, ProfileArn: f.awsProfile, RoleArn: f.awsRole}
	}
	return nil
}

// write writes the user data, and with --network-config the network config beside it.
func (f *nodeFlags) write(stdout io.Writer) error {
	userData, err := f.n.UserData()
	if err != nil {
		return err
	}
	var nc []byte
	if f.networkConfig != "" {
		if nc, err = f.n.NetworkConfig(); err != nil {
			return err
		}
	}
	if err := pki.WriteFile(f.out, userData, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s", f.out)
	if nc != nil {
		if err := os.WriteFile(f.networkConfig, nc, 0o644); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, " and %s", f.networkConfig)
	}
	_, _ = fmt.Fprintln(stdout)
	return nil
}
