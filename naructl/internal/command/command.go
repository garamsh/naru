// Package command is naructl's command line: the gen commands, topology, and the Node API's client commands.
package command

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/naructl/internal/layout"
	"github.com/garamsh/naru/naructl/internal/pki"
)

const usage = `usage: naructl gen <ca|client|token|node|api-ca|api-client|host-ca|host-client> [flags]
       naructl <version|services|logs|get config|apply|reboot|upgrade> --config <client file> [--node <ip[:port]>] [flags]
       naructl operator <add|revoke> --admin <person file> --passphrase-file <file> --socket <path> [flags]
                                                     issue or revoke an operator through naru-signer (naructl operator -h)
       naructl operator request|person, naructl orch csr|issue, naructl signer authorise-replica|bundle|restore
                                                     make an identity's key, request and person file, and use root's (naructl operator -h, naructl signer -h)
       naructl topology [kubernetes]                 print the services a Node runs, and which signs, as JSON
       naructl console --orch <url> --identity <person file> --passphrase-file <file> [--listen 127.0.0.1:8484] [--ca <file>]
              serve a web console on this workstation; it signs admissions with your key, which stays in this process

  gen api-ca  --ca-dir <dir>                    write the Node API authority, <dir>/api/ca.pem and ca.key
  gen api-client --ca-dir <dir> --role admin|reader --endpoint <ip[:port]> --out <file>
              write a client file for the Node API
  gen host-ca --ca-dir <dir>                    write the host API authority, <dir>/host/ca.pem and ca.key
  gen host-client --ca-dir <dir> --role provisioner|reader --endpoint <ip[:port]> --out <file>
              write a client file for the host API (naru-hostd, port 7443)
  host | microvm list | microvm get <name> --config <host client file> [--host <ip[:port]>]
  microvm create (--name <name> --image <repository@sha256:digest> [--cpu n] [--memory bytes] [--root-disk bytes] [--disk <name>=<bytes>[,ro]] | -f <request.json>)
  microvm create --name <name> --boot-set <set> --seed <user-data file> --data-disk <bytes> [--cmdline <args>] [--pod-prefix <cidr>] [--cpu n] [--memory bytes]
              the OS form, in place of --image, --root-disk and --disk
  microvm stop <name> | microvm start <name> | microvm delete <name>
  host boot-set add <naru-node-<version>.json> | host boot-set remove <name>
              add a release's kernel and initramfs (the files its manifest names, beside it) to the host
              as a boot set with no reboot, or remove one
              all of these take the same flags; create, stop, start, delete and boot-set need the provisioner role
  orch init --out-dir <dir> --passphrase-file <file>    write the recovery key, encrypted, and its public half
  orch backup [--out <file>]                            take a sealed snapshot now; with --out, also fetch it
  orch restore --snapshot <file> --recovery-key <file> --passphrase-file <file> [--out <file>]
              [--push <orch URL> --orch-ca <file> --admin <root person file> [--admin-passphrase-file <file>]]
              open the sealed snapshot; --out writes the plain etcd snapshot, --push sends it to a naru-orchd
              started with --restore (the plain snapshot is not written to disk without --out)
  enroll-key create [--ttl d] | enroll-key revoke <id>  | host pending | host admit <id> --code <code> --region <r> [--label k=v]
  host label <id> --region <r> [--label k=v] | host drain <id> | host undrain <id> | group apply -f <spec> | group get <name> | group list | group delete <name>
              these take --orch-url, --orch-ca, --identity and --passphrase-file
  version | services | get config               read a Node
  logs <service> [-f]                           print a service's kept log, and with -f each new line
  apply -f <patch> [--mode no-reboot|staged|reboot] [--dry-run]
              merge a patch into the Node's configuration
  reboot      stop the Node's services and reboot it
  upgrade --bundle <naru-upgrade-<version>.tar> --sums <SHA256SUMS>
              check the bundle against the sums file, then send it: the Node reboots into it once, and keeps it when healthy
  upgrade --status                              print the Node's boot slot and how its last upgrade ended
  gen ca      --out <dir>                       write ca.pem and ca.key
  gen client  --ca <dir> --cn <name> --out <dir> write an operator's client.pem and client.key
  gen etcd-client --ca <dir> --out <dir>      write ca.crt, tls.crt and tls.key, a client pair etcd's members accept
  gen token   --ca <dir> --name <name>              write the Node's bootstrap token to <dir>/tokens/<name>, if none is there
  gen node    --ca <dir> --name <name> --address <cidr> --mac <mac> --gateway <ip> --nameserver <ip>
              --api-server <ip> --machine-prefix <cidr> --guest <guest.json> --firecracker-sha256 <hex>
              --out <file> [--control-plane] [--peer <address>=<machine-prefix>] [--time-server <host[:port]>] [more flags: naructl gen node -h]
` + kubernetesUsage

// Run runs naructl with args, the command line without the program's name.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) >= 1 && args[0] == "topology" && len(args) <= 2 {
		t := layout.Gaon
		if len(args) == 2 {
			if args[1] != "kubernetes" {
				return fmt.Errorf("topology %q: the one besides gaon's is kubernetes", args[1])
			}
			t = layout.Kubernetes
		}
		b, err := t.JSON()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", b)
		return err
	}
	if handled, err := orchCommands(args, stdout, stderr); handled {
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case "version":
			return version(args[1:], stdout, stderr)
		case "services":
			return services(args[1:], stdout, stderr)
		case "logs":
			return logs(args[1:], stdout, stderr)
		case "apply":
			return apply(args[1:], stdout, stderr)
		case "reboot":
			return reboot(args[1:], stdout, stderr)
		case "upgrade":
			return upgradeCmd(args[1:], stdout, stderr)
		case "host":
			return hostCmd(args[1:], stdout, stderr)
		case "console":
			return consoleCmd(args[1:], stdout, stderr)
		case "microvm":
			return microvm(args[1:], stdout, stderr)
		case "operator":
			return operatorCmd(args[1:], stdout, stderr)
		case "signer":
			return signerCmd(args[1:], stdout, stderr)
		case "get":
			if len(args) > 1 && args[1] == "config" {
				return getConfig(args[2:], stdout, stderr)
			}
		}
	}
	if len(args) < 2 || args[0] != "gen" {
		_, _ = fmt.Fprint(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[1] {
	case "api-ca":
		return genAPICA(args[2:], stdout, stderr)
	case "api-client":
		return genAPIClient(args[2:], stdout, stderr)
	case "host-ca":
		return genHostCA(args[2:], stdout, stderr)
	case "host-client":
		return genHostClient(args[2:], stdout, stderr)
	case "ca":
		return genCA(args[2:], stdout, stderr)
	case "client":
		return genClient(args[2:], stdout, stderr)
	case "etcd-client":
		return genEtcdClient(args[2:], stdout, stderr)
	case "token":
		return genToken(args[2:], stdout, stderr)
	case "node":
		return genNode(args[2:], stdout, stderr)
	case "kubernetes":
		return genKubernetes(args[2:], stdout, stderr)
	case "kubernetes-node":
		return genKubernetesNode(args[2:], stdout, stderr)
	}
	_, _ = fmt.Fprint(stderr, usage)
	return fmt.Errorf("unknown command %q", "gen "+args[1])
}

// list is a flag that may be given more than once.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("naructl "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func require(fs *flag.FlagSet, names ...string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for _, n := range names {
		if !set[n] {
			return fmt.Errorf("--%s is required", n)
		}
	}
	return nil
}

// needs refuses each of the flags given without the flag that makes it apply, which would otherwise
// be accepted and do nothing.
func needs(fs *flag.FlagSet, with string, flags ...string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set[with] {
		return nil
	}
	for _, f := range flags {
		if set[f] {
			return fmt.Errorf("--%s has no effect without --%s", f, with)
		}
	}
	return nil
}

func genCA(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen ca", stderr)
	out := fs.String("out", "", "directory that receives ca.pem and ca.key (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "out"); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*out, pki.KeyFile)); err == nil {
		return fmt.Errorf("%s already holds an authority; a new one would replace its key", *out)
	}
	ca, err := pki.New("naru-ca")
	if err != nil {
		return err
	}
	if err := ca.Write(*out); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s and %s; keep %s in your secret store\n", filepath.Join(*out, pki.CertFile), filepath.Join(*out, pki.KeyFile), pki.KeyFile)
	return nil
}

func genClient(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen client", stderr)
	caDir := fs.String("ca", "", "directory of ca.pem and ca.key")
	cn := fs.String("cn", "", "the certificate's CN, the caller's identity to gaon")
	out := fs.String("out", "", "directory that receives client.pem and client.key (0600)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "cn", "out"); err != nil {
		return err
	}
	ca, err := pki.Load(*caDir)
	if err != nil {
		return err
	}
	pair, err := ca.Client(*cn)
	if err != nil {
		return err
	}
	if err := pki.WritePair(*out, "client", pair); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s and %s for CN %s\n", filepath.Join(*out, "client.pem"), filepath.Join(*out, "client.key"), *cn)
	return nil
}

// EtcdClientCN is the subject CN of the pair gen etcd-client issues: the caller of etcd's client
// port that is gaon's snapshot writer (ADR 0024). etcd authorizes no CN, any pair the CA signed is
// accepted, so the name is for whoever reads etcd's logs.
const EtcdClientCN = "gaon-etcd-snapshot"

func genEtcdClient(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen etcd-client", stderr)
	caDir := fs.String("ca", "", "directory of ca.pem and ca.key")
	out := fs.String("out", "", "directory that receives ca.crt, tls.crt and tls.key (0600), the keys of gaon's Secret etcd-snapshot-etcd-client")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "out"); err != nil {
		return err
	}
	ca, err := pki.Load(*caDir)
	if err != nil {
		return err
	}
	pair, err := ca.Client(EtcdClientCN)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil { //nolint:gosec // holds a certificate
		return err
	}
	if err := pki.WriteFile(filepath.Join(*out, "tls.crt"), pair.CertPEM, 0o644); err != nil { //nolint:gosec // a certificate
		return err
	}
	if err := pki.WriteFile(filepath.Join(*out, "tls.key"), pair.KeyPEM, 0o600); err != nil {
		return err
	}
	// etcd's members trust the CA and serve with certificates it signed: the one file is both the
	// CA that signs their server certificates and the CA their client certificates are checked against.
	if err := pki.WriteFile(filepath.Join(*out, "ca.crt"), ca.CertPEM, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s, %s and %s for CN %s\n", filepath.Join(*out, "ca.crt"), filepath.Join(*out, "tls.crt"), filepath.Join(*out, "tls.key"), EtcdClientCN)
	return nil
}

func genToken(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen token", stderr)
	caDir := fs.String("ca", "", "directory of the cluster's authority, which holds tokens/")
	name := fs.String("name", "", "the Node's name, such as node-2")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "name"); err != nil {
		return err
	}
	if _, err := pki.Load(*caDir); err != nil {
		return err
	}
	if _, err := pki.EnsureToken(*caDir, *name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: %s holds the token of %s\n", filepath.Join(*caDir, pki.TokensDir, *name), *name)
	return nil
}

func genNode(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("gen node", stderr)
	var (
		nameservers, serving, binaries, disable, timeServers, peers, members, registries, storage, noMasquerade list
		awsRegion, awsTrustAnchor, awsProfile, awsRole, hostRegion                                              string
		hostLabels, orchCluster, orchSANs, signerPeerNames                                                      list
		hostMgmt, hostEgress, signerPeer, signerRecovery, signerRootCSR, metricsListen                          string
		orchPeerURL, orchClientURL, orchState, orchListen, orchCert, orchCA, orchRevocation                     string
		orchRecovery, orchKey, orchHostCA, hostClientCA, orchJoin                                               string
		hostTunnelPort                                                                                          int
		hostOrchURL, hostOrchCA, hostOrchKey, signerJoinPeer, signerJoinPeerCA, signerJoinAuthz                 string
		n                                                                                                       layout.Node
		caDir, address, gateway, apiServer                                                                      string
		machinePrefix, guest, extraNIC                                                                          string
		out, networkConfig                                                                                      string
		controlPlane, pairsAsFiles, runSigner, signerReplica, orchRestore                                       bool
	)
	fs.StringVar(&caDir, "ca", "", "directory of ca.pem and ca.key (the key is read here and never carried by a worker)")
	fs.StringVar(&n.Name, "name", "", "the Node's name, such as node-1")
	fs.BoolVar(&controlPlane, "control-plane", false, "the Node runs etcd and gaon's control plane, issues its certificates at boot and signs its agents' requests")
	fs.StringVar(&address, "address", "", "the Node's address and prefix, such as 192.168.0.40/24")
	fs.StringVar(&n.MAC, "mac", "", "the MAC address of the Node's card (with --network-config)")
	fs.StringVar(&gateway, "gateway", "", "the default gateway (with --network-config)")
	fs.Var(&nameservers, "nameserver", "a nameserver, repeatable (with --network-config)")
	fs.StringVar(&n.Search, "search", "", "the search domain (with --network-config)")
	fs.StringVar(&extraNIC, "extra-nic", "", "a second card, <mac>=<cidr>, with no gateway (with --network-config)")
	fs.StringVar(&apiServer, "api-server", "", "the address every agent and naructl dial: the control-plane Node's, or the virtual address of several (--member)")
	fs.Var(&members, "member", "<name>=<address>, a control-plane Node of a control plane of several, repeatable, in priority order and the same on every member, this Node among them (with --control-plane)")
	fs.Var(&serving, "serving-address", "another address the API server's certificate and the Node API's certificate name, repeatable")
	fs.Var(&binaries, "binary", "<service>=<absolute path>, the executable of a service in place of the image's, repeatable")
	fs.Var(&disable, "disable", "a service to leave out ("+strings.Join(layout.Gaon.Names(), ", ")+"), repeatable")
	fs.Var(&timeServers, "time-server", "an SNTP server init sets the clock from, host or host:port, repeatable and asked in order (none: the clock is not set)")
	fs.Var(&registries, "registry", "a registry mvmd pulls from, host=<host[:port]> then any of ca=<PEM file>, username=<name> with password-file=<file>, or helper=<name> (a username with a helper is refused), comma-separated, repeatable")
	fs.StringVar(&awsRegion, "aws-region", "", "the AWS region of the IAM Roles Anywhere profile the credential helpers use (with the other --aws flags: all four or none)")
	fs.StringVar(&awsTrustAnchor, "aws-trust-anchor", "", "the ARN of the IAM Roles Anywhere trust anchor")
	fs.StringVar(&awsProfile, "aws-profile", "", "the ARN of the IAM Roles Anywhere profile")
	fs.StringVar(&awsRole, "aws-role", "", "the ARN of the IAM role; the certificate and key are the Node Agent's pair")
	fs.StringVar(&hostRegion, "host-region", "", "the Node serves the host API (naru-hostd) and reports this region, a DNS label; needs the host authority, gen host-ca")
	fs.Var(&hostLabels, "host-label", "a label of the host, <key>=<value>, repeatable (with --host-region)")
	fs.StringVar(&hostMgmt, "host-management-prefix", "", "spec.host.network: the prefix of the management plane, which no virtual network may reach, such as 192.168.0.0/24 (with --host-region)")
	fs.StringVar(&hostEgress, "host-egress-interface", "", "spec.host.network: the interface the virtual networks' egress leaves by (with --host-management-prefix)")
	fs.IntVar(&hostTunnelPort, "host-tunnel-port", 0, "spec.host.network: the UDP port of the WireGuard tunnel (with --host-management-prefix; default 51820)")
	fs.BoolVar(&runSigner, "signer", false, "the Node runs naru-signer (spec.signer)")
	fs.StringVar(&signerPeer, "signer-peer-listen", "", "spec.signer: the address of the peer listener other signers replicate from (with --signer-peer-name)")
	fs.Var(&signerPeerNames, "signer-peer-name", "spec.signer: a name or address of the peer listener's certificate, repeatable")
	fs.StringVar(&hostOrchURL, "host-orch-url", "", "spec.host.orch: the https URL of orch's API, which naru-hostd enrolls with (with --host-region)")
	fs.StringVar(&hostOrchCA, "host-orch-ca-sha256", "", "spec.host.orch: the hex SHA-256 of the authority orch's certificate chains to")
	fs.StringVar(&hostOrchKey, "host-orch-enroll-key", "", "spec.host.orch: the file holding the enrollment key, which the user data carries 0600 for naru-hostd alone")
	fs.BoolVar(&signerReplica, "signer-replica", false, "spec.signer.replica: the signer is a replica of another and makes its join key at its first start (excludes --signer-root-csr)")
	fs.StringVar(&signerJoinPeer, "signer-join-peer", "", "spec.signer.replica.join: host:port of the peer listener of a signer that holds the authorities (with --signer-replica)")
	fs.StringVar(&signerJoinPeerCA, "signer-join-peer-ca", "", "spec.signer.replica.join: the file holding the PEM of the orch authority the peer's certificate chains to")
	fs.StringVar(&signerJoinAuthz, "signer-join-authorisation", "", "spec.signer.replica.join: the file holding root's authorisation, which naructl signer authorise-replica wrote")
	fs.StringVar(&signerRecovery, "signer-recovery-public", "", "spec.signer.bootstrap: the file holding the recovery key's public half (with --signer-root-csr)")
	fs.StringVar(&signerRootCSR, "signer-root-csr", "", "spec.signer.bootstrap: the file holding the PEM certificate request of the first root identity")
	fs.StringVar(&orchPeerURL, "orch-peer-url", "", "spec.orch: the https URL etcd listens on for its peers; giving it makes the Node run naru-orchd, which needs the other --orch flags and --signer")
	fs.StringVar(&orchClientURL, "orch-client-url", "", "spec.orch: the https URL etcd listens on for clients")
	fs.Var(&orchCluster, "orch-member", "spec.orch: <name>=<peer URL>, a member of orch's cluster, this Node among them, repeatable")
	fs.StringVar(&orchState, "orch-state", "", "spec.orch: new, or existing for a member that joins a cluster (default new)")
	fs.StringVar(&orchListen, "orch-listen", "", "spec.orch: the address of orch's HTTPS API (default 0.0.0.0:7500)")
	fs.Var(&orchSANs, "orch-san", "spec.orch: a name or address of orch's certificate, repeatable")
	fs.StringVar(&orchCert, "orch-certificate", "", "spec.orch: the file holding orch's certificate then the authority's, PEM")
	fs.StringVar(&orchCA, "orch-ca", "", "spec.orch: the file holding the authority's certificate, PEM")
	fs.StringVar(&orchRevocation, "orch-revocation-public-key", "", "spec.orch: the file holding the revocation public key, PKIX PEM, which the signer's init printed")
	fs.StringVar(&orchHostCA, "orch-host-ca", "", "spec.orch: the file holding the PEM of the authority the hosts' API certificates chain to, the host authority; without it orch trusts --orch-ca for them")
	fs.StringVar(&hostClientCA, "host-client-ca", "", "spec.host: the file holding the PEM of the authority orch's certificate chains to, which naru-hostd trusts for its clients beside the host authority (with --host-region)")
	fs.StringVar(&orchRecovery, "orch-recovery-public", "", "spec.orch: the file holding the recovery key's public half; without it orch writes no snapshot")
	fs.StringVar(&orchJoin, "orch-join", "", "spec.orch.join: the client URL, https://<host>:<port>, of a running member, which this member joins; it takes the initial cluster from that cluster, so no --orch-member, and excludes --orch-restore")
	fs.BoolVar(&orchRestore, "orch-restore", false, "spec.orch.restore: orchd starts empty and waits for root's etcd snapshot (PUT /v1/restore)")
	fs.StringVar(&orchKey, "orch-key", "", "spec.orch: the file holding orch's private key, which the user data carries 0600 for orch's user alone")
	fs.StringVar(&metricsListen, "metrics-listen", "", "spec.metrics: the IP address init and the daemons serve /metrics on, each on its own port")
	fs.StringVar(&machinePrefix, "machine-prefix", "", "the IPv4 prefix mvmd gives its Machines, such as 10.200.1.0/24")
	fs.Var(&peers, "peer", "another Node of the cluster, <address>=<machine-prefix> such as 192.168.0.61=10.200.2.0/24, repeatable: the Node routes to the prefix via the address, and its Machines keep their source address to it")
	fs.Var(&noMasquerade, "no-masquerade", "an IPv4 prefix in canonical form, repeatable: a Machine's traffic to it keeps its source address (mvmd's network.no_masquerade) and the Node gets no route to it, such as the Machine prefixes of the hosts a WireGuard tunnel reaches")
	fs.Var(&storage, "storage", "<name>=<label or /dev/path>, an extra disk the Node offers as XFS, found by its partition or file system label, or by its device when the value starts with /dev/ (a blank disk has no label), repeatable: the first is mounted where mvmd keeps its disks")
	fs.StringVar(&guest, "guest", "", "naru-guest-<version>.json, the guest manifest")
	fs.StringVar(&n.FirecrackerSHA256, "firecracker-sha256", "", "the SHA-256 of the Firecracker binary the image carries")
	fs.BoolVar(&pairsAsFiles, "pairs-as-files", false, "the Node carries operator-issued pairs as files, with no bootstrap token: on a control-plane Node every pair, and no signing key")
	fs.StringVar(&out, "out", "", "the user-data file to write (0600: it carries keys; a file already there is replaced)")
	fs.StringVar(&networkConfig, "network-config", "", "also write the NoCloud network-config to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := require(fs, "ca", "name", "address", "api-server", "machine-prefix", "guest", "firecracker-sha256", "out"); err != nil {
		return err
	}
	if err := needs(fs, "network-config", "mac", "gateway", "nameserver", "search", "extra-nic"); err != nil {
		return err
	}
	if err := needs(fs, "control-plane", "serving-address", "member"); err != nil {
		return err
	}
	if err := awsTogether(fs); err != nil {
		return err
	}
	if err := needs(fs, "host-region", "host-label", "host-client-ca"); err != nil {
		return err
	}
	if err := needs(fs, "host-management-prefix", "host-egress-interface", "host-tunnel-port"); err != nil {
		return err
	}
	if err := needs(fs, "host-orch-url", "host-orch-ca-sha256", "host-orch-enroll-key"); err != nil {
		return err
	}
	if err := needs(fs, "signer-replica", "signer-join-peer", "signer-join-peer-ca", "signer-join-authorisation"); err != nil {
		return err
	}
	if err := needs(fs, "signer", "signer-replica", "signer-peer-listen", "signer-peer-name", "signer-recovery-public", "signer-root-csr"); err != nil {
		return err
	}
	if err := needs(fs, "orch-peer-url", "orch-client-url", "orch-member", "orch-state", "orch-listen", "orch-san", "orch-certificate", "orch-ca", "orch-revocation-public-key", "orch-recovery-public", "orch-key", "orch-restore", "orch-join", "orch-host-ca"); err != nil {
		return err
	}
	var err error
	if n.CA, err = pki.Load(caDir); err != nil {
		return err
	}
	if err := genNodeDaemons(&n, daemonFlags{
		hostMgmt: hostMgmt, hostEgress: hostEgress, hostTunnelPort: hostTunnelPort,
		hostOrchURL: hostOrchURL, hostOrchCA: hostOrchCA, hostOrchKey: hostOrchKey,
		signerReplica: signerReplica, signerJoinPeer: signerJoinPeer, signerJoinPeerCA: signerJoinPeerCA, signerJoinAuthz: signerJoinAuthz,
		runSigner: runSigner, signerPeer: signerPeer, signerPeerNames: signerPeerNames, signerRecovery: signerRecovery, signerRootCSR: signerRootCSR,
		orchPeerURL: orchPeerURL, orchClientURL: orchClientURL, orchCluster: orchCluster, orchState: orchState, orchListen: orchListen, orchSANs: orchSANs,
		orchCert: orchCert, orchCA: orchCA, orchRevocation: orchRevocation, orchRecovery: orchRecovery, orchKey: orchKey, orchHostCA: orchHostCA, orchRestore: orchRestore, orchJoin: orchJoin, metricsListen: metricsListen,
	}); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(caDir, pki.APIDir)); err == nil {
		if n.API, err = pki.Load(filepath.Join(caDir, pki.APIDir)); err != nil {
			return fmt.Errorf("the Node API authority: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if hostRegion != "" {
		if n.Host, err = loadHost(caDir, hostRegion, hostLabels); err != nil {
			return err
		}
		if hostClientCA != "" {
			if n.Host.ClientCA, err = readText("--host-client-ca", hostClientCA); err != nil {
				return err
			}
		}
	}
	n.Mode = layout.ModeOf(controlPlane, pairsAsFiles)
	// The Node's own token is made if there is none. A control-plane Node's API server lists the
	// token of every Node whose file is in the directory now: run gen token for the others first.
	if n.Token, n.Tokens, err = n.Mode.LoadTokens(caDir, n.Name); err != nil {
		return err
	}
	if n.Address, err = netip.ParsePrefix(address); err != nil {
		return fmt.Errorf("--address: %w", err)
	}
	if n.APIServer, err = netip.ParseAddr(apiServer); err != nil {
		return fmt.Errorf("--api-server: %w", err)
	}
	if n.MachinePrefix, err = netip.ParsePrefix(machinePrefix); err != nil {
		return fmt.Errorf("--machine-prefix: %w", err)
	}
	if gateway != "" {
		if n.Gateway, err = netip.ParseAddr(gateway); err != nil {
			return fmt.Errorf("--gateway: %w", err)
		}
	}
	for _, s := range nameservers {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("--nameserver: %w", err)
		}
		n.Nameservers = append(n.Nameservers, a)
	}
	for _, s := range serving {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("--serving-address: %w", err)
		}
		n.ServingAddresses = append(n.ServingAddresses, a)
	}
	for _, s := range peers {
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
	for _, s := range noMasquerade {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return fmt.Errorf("--no-masquerade %q is not an IPv4 prefix in canonical form", s)
		}
		n.NoMasquerade = append(n.NoMasquerade, p)
	}
	for _, s := range storage {
		name, value, ok := strings.Cut(s, "=")
		if !ok {
			return fmt.Errorf("--storage %q is not <name>=<label or /dev/path>", s)
		}
		d := layout.StorageDisk{Name: name, Label: value}
		if strings.HasPrefix(value, "/dev/") {
			d = layout.StorageDisk{Name: name, Device: value}
		}
		n.Storage = append(n.Storage, d)
	}
	for _, m := range members {
		name, addr, ok := strings.Cut(m, "=")
		a, err := netip.ParseAddr(addr)
		if !ok || err != nil {
			return fmt.Errorf("--member %q is not <name>=<address>", m)
		}
		n.Members = append(n.Members, layout.Member{Name: name, Address: a})
	}
	if extraNIC != "" {
		mac, cidr, ok := strings.Cut(extraNIC, "=")
		if !ok {
			return fmt.Errorf("--extra-nic %q is not <mac>=<cidr>", extraNIC)
		}
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("--extra-nic: %w", err)
		}
		n.ExtraNIC = &layout.NIC{MAC: mac, Address: p}
	}
	n.Binaries = map[string]string{}
	for _, b := range binaries {
		name, bin, ok := strings.Cut(b, "=")
		if !ok {
			return fmt.Errorf("--binary %q is not <service>=<path>", b)
		}
		n.Binaries[name] = bin
	}
	n.Disable = disable
	n.TimeServers = timeServers
	for _, r := range registries {
		reg, err := parseRegistry(r)
		if err != nil {
			return fmt.Errorf("--registry: %w", err)
		}
		n.Registries = append(n.Registries, reg)
	}
	if awsRegion != "" {
		n.AWS = &layout.AWS{Region: awsRegion, TrustAnchorArn: awsTrustAnchor, ProfileArn: awsProfile, RoleArn: awsRole}
	}
	if n.Guest, err = layout.ReadManifest(guest); err != nil {
		return err
	}
	userData, err := n.UserData()
	if err != nil {
		return err
	}
	var nc []byte
	if networkConfig != "" {
		if nc, err = n.NetworkConfig(); err != nil {
			return err
		}
	}
	if err := pki.WriteFile(out, userData, 0o600); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "naructl: wrote %s", out)
	if nc != nil {
		if err := os.WriteFile(networkConfig, nc, 0o644); err != nil { //nolint:gosec // carries no key
			return err
		}
		_, _ = fmt.Fprintf(stdout, " and %s", networkConfig)
	}
	_, _ = fmt.Fprintln(stdout)
	return nil
}

// awsTogether refuses some of the four --aws flags without the others.
func awsTogether(fs *flag.FlagSet) error {
	names := []string{"aws-region", "aws-trust-anchor", "aws-profile", "aws-role"}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	n := 0
	for _, f := range names {
		if set[f] {
			n++
		}
	}
	if n != 0 && n != len(names) {
		return fmt.Errorf("give all of --%s, or none", strings.Join(names, ", --"))
	}
	return nil
}

// parseRegistry reads one --registry value: host=<host> and the keys ca, username, password-file
// and helper, comma-separated. The CA and the password are read from their files here, so the
// user-data carries them and the Node reads no path of the operator's.
func parseRegistry(v string) (layout.Registry, error) {
	var r layout.Registry
	var caFile, passwordFile string
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" {
			return r, fmt.Errorf("%q is not key=value", kv)
		}
		switch k {
		case "host":
			r.Host = val
		case "ca":
			caFile = val
		case "username":
			r.Username = val
		case "password-file":
			passwordFile = val
		case "helper":
			r.Helper = val
		default:
			return r, fmt.Errorf("the key %q is not one of host, ca, username, password-file, helper", k)
		}
	}
	if r.Host == "" {
		return r, errors.New("host=<host> is required")
	}
	if r.Helper != "" && (r.Username != "" || passwordFile != "") {
		return r, fmt.Errorf("%s gives both a helper and a username or password; give one", r.Host)
	}
	if (r.Username == "") != (passwordFile == "") {
		return r, fmt.Errorf("%s needs both username and password-file", r.Host)
	}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return r, err
		}
		r.CA = string(b)
	}
	if passwordFile != "" {
		b, err := os.ReadFile(passwordFile)
		if err != nil {
			return r, err
		}
		r.Password = strings.TrimRight(string(b), "\r\n")
	}
	return r, nil
}
