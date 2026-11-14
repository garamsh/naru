package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// What init gives the daemons it runs from spec.orch, spec.signer, spec.host.network and
// spec.metrics (docs/architecture/adr/0050-the-image-carries-orch-and-the-signer-and-init-runs-them-as-separate-users-from-spec-orch-spec-signer-and-spec-metrics.md).
const (
	// HostdService, OrchService and SignerService are the names of the services; the last two are
	// derived from their sections, so no spec.services entry may keep them.
	HostdService  = "naru-hostd"
	OrchService   = "naru-orchd"
	SignerService = "naru-signer"

	hostdBinary   = "/bin/naru-hostd"
	orchBinary    = "/bin/naru-orchd"
	signerBinary  = "/bin/naru-signer"
	defaultTunnel = 51820

	// SignerUID is the user and group naru-signer runs as, and the group orch joins to reach its
	// socket; OrchUID is the user and group naru-orchd runs as.
	SignerUID uint32 = 6001
	OrchUID   uint32 = 6002

	// SignerDir holds the authority keys: mode 0700 and owned by SignerUID, so orch's user cannot read it.
	SignerDir = "/var/lib/naru/signer"
	// SignerRunDir holds the signer's socket and the files of spec.signer.bootstrap.
	SignerRunDir = "/run/naru-signer"
	// OrchDataDir holds orch's etcd data and snapshots: mode 0700 and owned by OrchUID.
	OrchDataDir = "/var/lib/naru/orch"
	// OrchRunDir holds orch's serving pair, authority and public keys.
	OrchRunDir = "/run/naru-orchd"
	// OrchKeyFile is orch's private key. It is the one file of this directory that spec.files
	// carries, so GET /v1/config redacts it; spec.orch has no field for a key.
	OrchKeyFile = OrchRunDir + "/orch.key"

	// HostdEnrollKeyFile is where init writes spec.host.orch.enrollKey, 0600 and root's, which is hostd's.
	HostdEnrollKeyFile = "/run/naru-hostd/enroll.key"

	// The ports of the metrics listeners, on the address spec.metrics.listen names.
	InitMetricsPort   = 9100
	HostdMetricsPort  = 9101
	OrchMetricsPort   = 9102
	SignerMetricsPort = 9103
)

// Orch is the orch member this Node runs. Its private key is the spec.files entry at OrchKeyFile.
type Orch struct {
	// Name is the member's name in the etcd cluster. It is metadata.name when empty.
	Name string `yaml:"name,omitempty"`
	// PeerURL and ClientURL are the https URLs etcd listens on.
	PeerURL   string `yaml:"peerURL"`
	ClientURL string `yaml:"clientURL"`
	// InitialCluster maps each member's name to its peer URL, this member's among them.
	InitialCluster map[string]string `yaml:"initialCluster"`
	// InitialClusterState is "new", or "existing" for a member that joins a cluster. It is "new" when empty.
	InitialClusterState string `yaml:"initialClusterState,omitempty"`
	// Listen is the address of orch's HTTPS API. It is "0.0.0.0:7500" when empty.
	Listen string `yaml:"listen,omitempty"`
	// SANs are the names and addresses of the member's certificate. They are ["127.0.0.1"] when empty.
	SANs []string `yaml:"sans,omitempty"`
	// Certificate is the PEM of orch's certificate followed by the authority's, CA the PEM of the
	// authority, and RevocationPublicKey the PKIX PEM of the key that signs the revocation list.
	Certificate         string `yaml:"certificate"`
	CA                  string `yaml:"ca"`
	RevocationPublicKey string `yaml:"revocationPublicKey"`
	// RecoveryPublicKey is the recovery key's public half, as the signer's init takes it. Without
	// it orch writes no snapshot.
	RecoveryPublicKey string `yaml:"recoveryPublicKey,omitempty"`
	// Restore starts the member with --restore: with an empty data directory it waits for root to
	// PUT an etcd snapshot to /v1/restore instead of starting a new cluster.
	Restore bool `yaml:"restore,omitempty"`
}

// SignerSection is the signer this Node runs (spec.signer).
type SignerSection struct {
	// PeerListen is the address of the peer listener other signers replicate from. It is empty when none is served.
	PeerListen string `yaml:"peerListen,omitempty"`
	// PeerNames are the DNS names and addresses of the peer listener's serving certificate; they
	// are required with PeerListen.
	PeerNames []string `yaml:"peerNames,omitempty"`
	// Bootstrap initialises a signer whose directory holds no authorities, at its first start.
	Bootstrap *SignerBootstrap `yaml:"bootstrap,omitempty"`
	// Replica makes the signer a replica of another. It excludes Bootstrap.
	Replica *SignerReplica `yaml:"replica,omitempty"`
}

// SignerBootstrap is what `naru-signer init` takes.
type SignerBootstrap struct {
	// RecoveryPublicKey is the recovery key's public half, a JSON document.
	RecoveryPublicKey string `yaml:"recoveryPublicKey"`
	// RootCSR is the PEM certificate request of the first root identity.
	RootCSR string `yaml:"rootCSR"`
}

// SignerReplica makes a signer that holds no authorities a replica of another signer. At its
// first start init runs `naru-signer join-key`, which logs the public join key on the console; once
// root has authorised that key (naructl signer authorise-replica), Join gives init what `naru-signer
// join` takes. The two steps are two applies, and neither runs once the directory holds authorities.
type SignerReplica struct {
	Join *SignerJoin `yaml:"join,omitempty"`
}

// SignerJoin is what `naru-signer join` takes.
type SignerJoin struct {
	// Peer is host:port of the peer listener (spec.signer.peerListen) of a signer that holds the authorities.
	Peer string `yaml:"peer"`
	// PeerCA is the PEM of the orch authority the peer's serving certificate chains to.
	PeerCA string `yaml:"peerCA"`
	// Authorisation is root's signed authorisation of this Node's join key, as naructl wrote it, for
	// the host ID that is metadata.name.
	Authorisation string `yaml:"authorisation"`
}

// HostOrch is spec.host.orch: what naru-hostd enrolls with orch from.
type HostOrch struct {
	// URL is the https URL of orch's API.
	URL string `yaml:"url"`
	// CASHA256 is the hex SHA-256 of the authority orch's certificate chains to.
	CASHA256 string `yaml:"caSHA256"`
	// EnrollKey is the enrollment key. init writes it 0600 to HostdEnrollKeyFile, and GET /v1/config redacts it.
	EnrollKey string `yaml:"enrollKey"`
}

// Metrics is where init and the daemons it starts serve /metrics.
type Metrics struct {
	// Listen is an IP address; each listener's port is fixed, by the constants above.
	Listen string `yaml:"listen"`
}

// HostNetwork is what naru-hostd's virtual networks are kept from.
type HostNetwork struct {
	// ManagementPrefix is the prefix of the management plane, which no network may reach.
	ManagementPrefix netip.Prefix `yaml:"managementPrefix"`
	// EgressInterface is the interface the networks' egress leaves by. Networks have none when empty.
	EgressInterface string `yaml:"egressInterface,omitempty"`
	// TunnelPort is the UDP port of the WireGuard tunnel. It is 51820 when 0.
	TunnelPort int `yaml:"tunnelPort,omitempty"`
}

// MetricsAddr returns the address init serves its own /metrics on, or "" when spec.metrics is not set.
func (s Spec) MetricsAddr() string {
	if s.Metrics == nil {
		return ""
	}
	return net.JoinHostPort(s.Metrics.Listen, strconv.Itoa(InitMetricsPort))
}

func (o Orch) name(node string) string {
	if o.Name == "" {
		return node
	}
	return o.Name
}

func (o Orch) listen() string {
	if o.Listen == "" {
		return "0.0.0.0:7500"
	}
	return o.Listen
}

func (o Orch) state() string {
	if o.InitialClusterState == "" {
		return "new"
	}
	return o.InitialClusterState
}

func (n HostNetwork) port() int {
	if n.TunnelPort == 0 {
		return defaultTunnel
	}
	return n.TunnelPort
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.Port() != "" && (u.Path == "" || u.Path == "/")
}

func pemOf(s, typ string) bool {
	b, _ := pem.Decode([]byte(s))
	return b != nil && (typ == "" || b.Type == typ)
}

func (v *validation) orch(cfg NodeConfig) {
	o := cfg.Spec.Orch
	if o == nil {
		return
	}
	if cfg.Spec.Signer == nil {
		v.fail("spec.orch", "needs spec.signer: orch asks the signer on this Node, over its socket, for every identity")
	}
	name := o.name(cfg.Metadata.Name)
	if !nameRE.MatchString(name) {
		v.fail("spec.orch.name", "must be lowercase letters, digits and '-', at most 63 characters, got %q", name)
	}
	for field, u := range map[string]string{"spec.orch.peerURL": o.PeerURL, "spec.orch.clientURL": o.ClientURL} {
		if !httpsURL(u) {
			v.fail(field, "must be an https URL with a host and a port, got %q", u)
		}
	}
	switch peer, ok := o.InitialCluster[name]; {
	case len(o.InitialCluster) == 0:
		v.fail("spec.orch.initialCluster", "must name the members of the cluster, this one among them")
	case !ok:
		v.fail("spec.orch.initialCluster", "must hold this member, %q", name)
	case peer != o.PeerURL:
		v.fail("spec.orch.initialCluster", "holds %q for this member, which is not spec.orch.peerURL %q", peer, o.PeerURL)
	}
	for member, u := range o.InitialCluster {
		if !nameRE.MatchString(member) || strings.ContainsAny(member, "=,") {
			v.fail("spec.orch.initialCluster", "has the invalid member name %q", member)
		}
		if !httpsURL(u) {
			v.fail("spec.orch.initialCluster", "has the peer URL %q for %q, which is not an https URL with a host and a port", u, member)
		}
	}
	if s := o.InitialClusterState; s != "" && s != "new" && s != "existing" {
		v.fail("spec.orch.initialClusterState", `must be "new" or "existing", got %q`, s)
	}
	if _, err := netip.ParseAddrPort(o.listen()); err != nil {
		v.fail("spec.orch.listen", "must be an IP address and a port, got %q", o.Listen)
	}
	for i, san := range o.SANs {
		if san == "" || strings.ContainsAny(san, ", ") {
			v.fail(fmt.Sprintf("spec.orch.sans[%d]", i), "must be a name or an address, got %q", san)
		}
	}
	if !pemOf(o.Certificate, "CERTIFICATE") {
		v.fail("spec.orch.certificate", "must be the PEM of a certificate, then the authority's")
	}
	if !pemOf(o.CA, "CERTIFICATE") {
		v.fail("spec.orch.ca", "must be the PEM of a certificate")
	}
	if !pemOf(o.RevocationPublicKey, "PUBLIC KEY") {
		v.fail("spec.orch.revocationPublicKey", "must be a PKIX public key in PEM")
	}
	if o.RecoveryPublicKey != "" && !json.Valid([]byte(o.RecoveryPublicKey)) {
		v.fail("spec.orch.recoveryPublicKey", "must be the JSON document of the recovery key's public half")
	}
	i := slices.IndexFunc(cfg.Spec.Files, func(f File) bool { return f.Path == OrchKeyFile })
	switch {
	case i < 0:
		v.fail("spec.orch", "needs a spec.files entry for %s, orch's private key, owned by uid %d, gid %d, mode 0600", OrchKeyFile, OrchUID, OrchUID)
	case cfg.Spec.Files[i].Perm()&0o077 != 0 || cfg.Spec.Files[i].Perm()&0o400 == 0:
		v.fail(fmt.Sprintf("spec.files[%d].mode", i), "must give the owner read and nobody else any access: it is orch's private key")
	case cfg.Spec.Files[i].Owner == nil || cfg.Spec.Files[i].Owner.UID != OrchUID:
		v.fail(fmt.Sprintf("spec.files[%d].owner", i), "must be uid %d: orch runs as it and reads this file", OrchUID)
	}
}

func (v *validation) signer(s *SignerSection) {
	if s == nil {
		return
	}
	if s.PeerListen != "" {
		if _, err := netip.ParseAddrPort(s.PeerListen); err != nil {
			v.fail("spec.signer.peerListen", "must be an IP address and a port, got %q", s.PeerListen)
		}
		if len(s.PeerNames) == 0 {
			v.fail("spec.signer.peerNames", "must name the peer listener's certificate with spec.signer.peerListen")
		}
	} else if len(s.PeerNames) > 0 {
		v.fail("spec.signer.peerNames", "is only used with spec.signer.peerListen")
	}
	for i, n := range s.PeerNames {
		if n == "" || strings.ContainsAny(n, ", ") {
			v.fail(fmt.Sprintf("spec.signer.peerNames[%d]", i), "must be a name or an address, got %q", n)
		}
	}
	if s.Bootstrap != nil && s.Replica != nil {
		v.fail("spec.signer.replica", "excludes spec.signer.bootstrap: a signer is initialised or replicated, not both")
	}
	if j := s.joinSection(); j != nil {
		if host, port, err := net.SplitHostPort(j.Peer); err != nil || host == "" || !numericPort(port) {
			v.fail("spec.signer.replica.join.peer", "must be host:port of a signer's peer listener, got %q", j.Peer)
		}
		if !pemOf(j.PeerCA, "CERTIFICATE") {
			v.fail("spec.signer.replica.join.peerCA", "must be the PEM of the orch authority's certificate")
		}
		if strings.TrimSpace(j.Authorisation) == "" {
			v.fail("spec.signer.replica.join.authorisation", "must hold root's signed authorisation, as naructl signer authorise-replica wrote it")
		}
	}
	if b := s.Bootstrap; b != nil {
		if !json.Valid([]byte(b.RecoveryPublicKey)) {
			v.fail("spec.signer.bootstrap.recoveryPublicKey", "must be the JSON document of the recovery key's public half")
		}
		if !pemOf(b.RootCSR, "CERTIFICATE REQUEST") {
			v.fail("spec.signer.bootstrap.rootCSR", "must be the PEM of a certificate request")
		}
	}
}

func (s SignerSection) joinSection() *SignerJoin {
	if s.Replica == nil {
		return nil
	}
	return s.Replica.Join
}

func numericPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n > 0 && n < 65536
}

func (v *validation) hostOrch(spec Spec) {
	h := spec.Host
	if h == nil || h.Orch == nil {
		return
	}
	o := h.Orch
	if !httpsURL(o.URL) {
		v.fail("spec.host.orch.url", "must be an https URL with a host and a port, got %q", o.URL)
	}
	if d, err := hex.DecodeString(o.CASHA256); err != nil || len(d) != sha256.Size {
		v.fail("spec.host.orch.caSHA256", "must be the SHA-256 of the authority, 64 hexadecimal digits")
	}
	if strings.TrimSpace(o.EnrollKey) == "" {
		v.fail("spec.host.orch.enrollKey", "must hold the enrollment key")
	}
	if !slices.ContainsFunc(spec.Services, func(s Service) bool { return s.Name == HostdService }) {
		v.fail("spec.host.orch", "needs the %s service in spec.services: its flags are written from this section", HostdService)
	}
}

func (v *validation) metrics(m *Metrics) {
	if m == nil {
		return
	}
	if a, err := netip.ParseAddr(m.Listen); err != nil || a.Zone() != "" {
		v.fail("spec.metrics.listen", "must be an IP address, got %q", m.Listen)
	}
}

func (v *validation) hostNetwork(spec Spec) {
	h := spec.Host
	if h == nil || h.Network == nil {
		return
	}
	n := h.Network
	if !n.ManagementPrefix.IsValid() {
		v.fail("spec.host.network.managementPrefix", "must be a prefix such as 192.168.0.0/24")
	} else if n.ManagementPrefix != n.ManagementPrefix.Masked() {
		v.fail("spec.host.network.managementPrefix", "has host bits set: %s", n.ManagementPrefix)
	}
	if e := n.EgressInterface; len(e) > 15 || strings.ContainsAny(e, "/ \x00") {
		v.fail("spec.host.network.egressInterface", "must be an interface name of at most 15 characters, got %q", e)
	}
	if n.TunnelPort < 0 || n.TunnelPort > 65535 {
		v.fail("spec.host.network.tunnelPort", "must be a port, 1 to 65535, or 0 for %d, got %d", defaultTunnel, n.TunnelPort)
	}
	if !slices.ContainsFunc(spec.Services, func(s Service) bool { return s.Name == HostdService }) {
		v.fail("spec.host.network", "needs the %s service in spec.services: its flags are written from this section", HostdService)
	}
}

// hostdOwned are the flags of naru-hostd that init writes, from spec.host.network and
// spec.metrics. A stored configuration holds them in the service's args, so Parse removes them
// and writes the ones the sections give: the flags follow the sections.
var hostdOwned = []string{"management-prefix", "egress-interface", "tunnel-port", "metrics-addr", "orch-url", "orch-ca-sha256", "enroll-key-file"}

func (v *validation) hostdArgs(services []Service) {
	for i, s := range services {
		if s.Name != HostdService {
			continue
		}
		for _, a := range s.Args {
			for _, f := range hostdOwned {
				if a == "--"+f || a == "-"+f {
					v.fail(fmt.Sprintf("spec.services[%d].args", i), "has --%s without a value in its own argument: init writes it as --%s=<value> from spec.host.network or spec.metrics", f, f)
				}
			}
		}
	}
}

// derive adds what the sections give to cfg: the files of spec.orch and spec.signer.bootstrap,
// the services naru-signer and naru-orchd, and the flags of naru-hostd. It first removes what an
// earlier Parse added, which a stored configuration carries, so Parse of its own output returns
// the same configuration.
func derive(cfg NodeConfig) NodeConfig {
	spec := cfg.Spec
	dirs := []string{OrchRunDir + "/", SignerRunDir + "/"}
	spec.Files = slices.DeleteFunc(slices.Clone(spec.Files), func(f File) bool {
		return f.Path == HostdEnrollKeyFile || f.Path != OrchKeyFile && slices.ContainsFunc(dirs, func(d string) bool { return strings.HasPrefix(f.Path, d) })
	})
	var services []Service
	for _, s := range spec.Services {
		switch s.Name {
		case SignerService, OrchService:
			continue
		case HostdService:
			s.Args = slices.DeleteFunc(slices.Clone(s.Args), func(a string) bool {
				return slices.ContainsFunc(hostdOwned, func(f string) bool { return strings.HasPrefix(a, "--"+f+"=") })
			})
			if h := spec.Host; h != nil && h.Network != nil {
				n := h.Network
				s.Args = append(s.Args, "--management-prefix="+n.ManagementPrefix.String(), "--tunnel-port="+strconv.Itoa(n.port()))
				if n.EgressInterface != "" {
					s.Args = append(s.Args, "--egress-interface="+n.EgressInterface)
				}
			}
			if h := spec.Host; h != nil && h.Orch != nil {
				s.Args = append(s.Args, "--orch-url="+h.Orch.URL, "--orch-ca-sha256="+h.Orch.CASHA256, "--enroll-key-file="+HostdEnrollKeyFile)
				spec.Files = append(spec.Files, File{Path: HostdEnrollKeyFile, Mode: "0600", Content: h.Orch.EnrollKey, Restart: []string{HostdService}})
			}
			if spec.Metrics != nil {
				s.Args = append(s.Args, "--metrics-addr="+net.JoinHostPort(spec.Metrics.Listen, strconv.Itoa(HostdMetricsPort)))
			}
		}
		services = append(services, s)
	}
	if sg := spec.Signer; sg != nil {
		svc, files := signerService(*sg, cfg.Metadata.Name, spec.Metrics)
		services = append(services, svc)
		spec.Files = append(spec.Files, files...)
	}
	if o := spec.Orch; o != nil {
		svc, files := orchService(*o, cfg.Metadata.Name, spec.Metrics)
		services = append(services, svc)
		spec.Files = append(spec.Files, files...)
	}
	spec.Services = services
	cfg.Spec = spec
	return cfg
}

func metricsFlag(m *Metrics, port int) []string {
	if m == nil {
		return nil
	}
	return []string{"--metrics-addr=" + net.JoinHostPort(m.Listen, strconv.Itoa(port))}
}

func signerService(s SignerSection, node string, m *Metrics) (Service, []File) {
	user := &User{UID: SignerUID, GID: SignerUID}
	args := []string{"serve", "--dir=" + SignerDir, "--socket=" + SignerRunDir + "/signer.sock", "--socket-mode=0660"}
	if s.PeerListen != "" {
		args = append(args, "--peer-listen="+s.PeerListen, "--peer-name="+strings.Join(s.PeerNames, ","))
	}
	svc := Service{
		Name: SignerService, Binary: signerBinary, Args: append(args, metricsFlag(m, SignerMetricsPort)...), User: user,
		Dirs: []Dir{
			{Path: SignerDir, Mode: "0700", UID: SignerUID, GID: SignerUID},
			{Path: SignerRunDir, Mode: "0750", UID: SignerUID, GID: SignerUID},
		},
	}
	var files []File
	if b := s.Bootstrap; b != nil {
		recovery, csr := SignerRunDir+"/recovery.pub", SignerRunDir+"/root.csr"
		files = []File{
			{Path: recovery, Mode: "0644", Content: b.RecoveryPublicKey},
			{Path: csr, Mode: "0644", Content: b.RootCSR},
		}
		svc.Setup = []Setup{{
			Args: []string{
				"init", "--dir=" + SignerDir, "--recovery-public=" + recovery, "--root-csr=" + csr,
				"--root-cert=" + SignerRunDir + "/root.crt", "--revocation-public-key=" + SignerRunDir + "/revocation.pub",
			},
			Unless: []string{SignerDir + "/recovery-public.json"},
		}}
	}
	if s.Replica != nil {
		authorities := SignerDir + "/recovery-public.json"
		pub := SignerRunDir + "/join.pub"
		svc.Setup = []Setup{{
			Args:   []string{"join-key", "--dir=" + SignerDir, "--out=" + pub},
			Unless: []string{SignerDir + "/join.key", authorities},
		}}
		if j := s.Replica.Join; j != nil {
			peerCA, authz := SignerRunDir+"/peer-ca.crt", SignerRunDir+"/replica.authorisation"
			files = []File{
				{Path: peerCA, Mode: "0644", Content: j.PeerCA},
				{Path: authz, Mode: "0644", Content: j.Authorisation},
			}
			svc.Setup = append(svc.Setup, Setup{
				Args:   []string{"join", "--dir=" + SignerDir, "--host-id=" + node, "--peer=https://" + j.Peer, "--peer-ca=" + peerCA, "--authorisation=" + authz},
				Unless: []string{authorities},
			})
		}
	}
	return svc, files
}

func orchService(o Orch, node string, m *Metrics) (Service, []File) {
	members := make([]string, 0, len(o.InitialCluster))
	for name, u := range o.InitialCluster {
		members = append(members, name+"="+u)
	}
	slices.Sort(members)
	sans := o.SANs
	if len(sans) == 0 {
		sans = []string{"127.0.0.1"}
	}
	args := []string{
		"--name=" + o.name(node), "--data-dir=" + OrchDataDir + "/etcd", "--peer-url=" + o.PeerURL, "--client-url=" + o.ClientURL,
		"--initial-cluster=" + strings.Join(members, ","), "--initial-cluster-state=" + o.state(), "--listen=" + o.listen(),
		"--san=" + strings.Join(sans, ","), "--cert=" + OrchRunDir + "/orch.crt", "--key=" + OrchKeyFile, "--ca=" + OrchRunDir + "/ca.crt",
		"--signer-socket=" + SignerRunDir + "/signer.sock", "--snapshot-dir=" + OrchDataDir + "/snapshots",
		"--revocation-public-key=" + OrchRunDir + "/revocation.pub",
	}
	if o.Restore {
		args = append(args, "--restore")
	}
	restart := []string{OrchService}
	files := []File{
		{Path: OrchRunDir + "/orch.crt", Mode: "0644", Content: o.Certificate, Restart: restart},
		{Path: OrchRunDir + "/ca.crt", Mode: "0644", Content: o.CA, Restart: restart},
		{Path: OrchRunDir + "/revocation.pub", Mode: "0644", Content: o.RevocationPublicKey, Restart: restart},
	}
	if o.RecoveryPublicKey != "" {
		args = append(args, "--recovery-public-key="+OrchRunDir+"/recovery.pub")
		files = append(files, File{Path: OrchRunDir + "/recovery.pub", Mode: "0644", Content: o.RecoveryPublicKey, Restart: restart})
	}
	return Service{
		Name: OrchService, Binary: orchBinary, Args: append(args, metricsFlag(m, OrchMetricsPort)...),
		User:            &User{UID: OrchUID, GID: OrchUID, Groups: []uint32{SignerUID}},
		Dirs:            []Dir{{Path: OrchDataDir, Mode: "0700", UID: OrchUID, GID: OrchUID}},
		RestartOnChange: []string{OrchKeyFile},
	}, files
}
