package config

import (
	"crypto/sha256"
	"crypto/x509"
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
	"time"

	"github.com/garamsh/naru/lib/wire"
)

// What init gives the daemons it runs from spec.orch, spec.signer, spec.host.network and
// spec.metrics (docs/architecture/adr/0050-the-image-carries-orch-and-the-signer-and-init-runs-them-as-separate-users-from-spec-orch-spec-signer-and-spec-metrics.md).
const (
	// HostdService, OrchService and SignerService are the names of the services; the last two are
	// derived from their sections, so no spec.services entry may keep them.
	HostdService  = "naru-hostd"
	OrchService   = "naru-orchd"
	SignerService = "naru-signer"
	// ProxyService is derived from spec.proxy, so no spec.services entry may keep it either.
	ProxyService = "naru-proxy"

	hostdBinary   = "/bin/naru-hostd"
	orchBinary    = ExtensionsRoot + "/" + OrchExtension + "/bin/naru-orchd"
	signerBinary  = ExtensionsRoot + "/" + OrchExtension + "/bin/naru-signer"
	proxyBinary   = "/bin/naru-proxy"
	defaultTunnel = 51820

	// SignerUID is the user and group naru-signer runs as, and the group orch joins to reach its
	// socket; OrchUID is the user and group naru-orchd runs as.
	SignerUID uint32 = 6001
	OrchUID   uint32 = 6002

	// SignerDir holds the authority keys: mode 0700 and owned by SignerUID, so orch's user cannot read it.
	SignerDir = wire.DataMount + "/signer"
	// SignerRunDir holds the signer's socket and the files of spec.signer.bootstrap.
	SignerRunDir = "/run/naru-signer"
	// SignerSocket is the unix socket naru-signer serves, the one orch dials and the Node API forwards /v1/signer to.
	SignerSocket = SignerRunDir + "/signer.sock"
	// OrchDataDir holds orch's etcd data and snapshots: mode 0700 and owned by OrchUID.
	OrchDataDir = wire.DataMount + "/orch"
	// OrchRunDir holds orch's serving pair, authority and public keys.
	OrchRunDir = "/run/naru-orchd"
	// OrchHostCAFile is the authority the hosts' API certificates chain to, from spec.orch.hostCA.
	OrchHostCAFile = OrchRunDir + "/host-ca.crt"
	// OrchKeyFile is orch's private key. It is the one file of this directory that spec.files
	// carries, so GET /v1/config redacts it; spec.orch has no field for a key.
	OrchKeyFile = OrchRunDir + "/orch.key"

	// HostdEnrollKeyFile is where init writes spec.host.orch.enrollKey, 0600 and root's, which is hostd's.
	HostdEnrollKeyFile = "/run/naru-hostd/enroll.key"

	// ProxyCertFile is the zone's certificate from spec.proxy.certificate, and ProxyKeyFile its
	// private key: the one file of the proxy's directory that spec.files carries, so GET /v1/config
	// redacts it.
	ProxyCertFile = wire.ProxyRunDir + "/zone.crt"
	ProxyKeyFile  = wire.ProxyRunDir + "/zone.key"

	// OrchBootSetTrustRootFile is where init writes spec.orch.bootSetTrustRoot when given; orchd
	// reads it with --boot-set-trust-root.
	OrchBootSetTrustRootFile = OrchRunDir + "/boot-set-trust-root.json"

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
	// InitialCluster maps each member's name to its peer URL, this member's among them. A member
	// that has Join takes it from the cluster it joins, so it has none.
	InitialCluster map[string]string `yaml:"initialCluster,omitempty"`
	// InitialClusterState is "new", or "existing" for a member that joins a cluster. It is "new"
	// when empty, and "existing" with Join.
	InitialClusterState string `yaml:"initialClusterState,omitempty"`
	// Join is the client URL, https://host:port, of a running member of the cluster. The member
	// adds itself to that cluster at its first start; init writes --join and the state "existing".
	// It excludes Restore.
	Join string `yaml:"join,omitempty"`
	// Listen is the address of orch's HTTPS API. It is "0.0.0.0:7500" when empty.
	Listen string `yaml:"listen,omitempty"`
	// MachineAPIListen is the address of the Machine API, served only with MachineZone. It is
	// "0.0.0.0:7600" when empty.
	MachineAPIListen string `yaml:"machineApiListen,omitempty"`
	// MachineZone is the zone of Machines' stable names, such as "m.example.com". Without it orch
	// serves no Machine API.
	MachineZone string `yaml:"machineZone,omitempty"`
	// SANs are the names and addresses of the member's certificate. They are ["127.0.0.1"] when empty.
	SANs []string `yaml:"sans,omitempty"`
	// Certificate is the PEM of orch's certificate followed by the authority's, CA the PEM of the
	// authority, and RevocationPublicKey the PKIX PEM of the key that signs the revocation list.
	Certificate         string `yaml:"certificate"`
	CA                  string `yaml:"ca"`
	RevocationPublicKey string `yaml:"revocationPublicKey"`
	// HostCA is the PEM of the authority the hosts' API certificates chain to. Orch presents its
	// own certificate to the hosts and trusts HostCA for theirs; without it orch trusts CA.
	HostCA string `yaml:"hostCA,omitempty"`
	// RecoveryPublicKey is the recovery key's public half, as the signer's init takes it. Without
	// it orch writes no snapshot.
	RecoveryPublicKey string `yaml:"recoveryPublicKey,omitempty"`
	// Restore starts the member with --restore: with an empty data directory it waits for root to
	// PUT an etcd snapshot to /v1/restore instead of starting a new cluster.
	Restore bool `yaml:"restore,omitempty"`
	// DepartAfter is how long the leader sees a member unreachable before it removes it (a Go
	// duration); orch's own 30m when empty (ADR 0088).
	DepartAfter string `yaml:"departAfter,omitempty"`
	// HousekeepingInterval is the pause between the leader's etcd housekeeping passes (a Go
	// duration); orch's own 1m when empty.
	HousekeepingInterval string `yaml:"housekeepingInterval,omitempty"`
	// EtcdQuotaBytes is the etcd backend quota in bytes, in place of the share of the data disk orch
	// derives; EtcdCompactionRetention is how much history etcd keeps (a Go duration), 10m when
	// empty. Both exist for tests (ADR 0088).
	EtcdQuotaBytes          int64  `yaml:"etcdQuotaBytes,omitempty"`
	EtcdCompactionRetention string `yaml:"etcdCompactionRetention,omitempty"`
	// BootSetStore is the https URL or directory path of the release assets the Node boot sets
	// are added from. Empty adds no boot sets (orchd's --boot-set-store).
	BootSetStore string `yaml:"bootSetStore,omitempty"`
	// BootSetTrustRoot is the content of the Sigstore trusted_root.json that verifies a release;
	// empty uses the public-good root orchd has built in (orchd's --boot-set-trust-root).
	BootSetTrustRoot string `yaml:"bootSetTrustRoot,omitempty"`
	// BootSetIdentity is the certificate identity a release is signed by; a trailing * is a
	// prefix of at least https://github.com/<owner>/<repo>/; {version} is the version asked for.
	// Empty uses orchd's default: the release workflow of garamsh/naru at the version's tag
	// (orchd's --boot-set-identity).
	BootSetIdentity string `yaml:"bootSetIdentity,omitempty"`
	// BootSetIssuer is the OIDC issuer of the certificate a release is signed with. Empty uses
	// orchd's default: https://token.actions.githubusercontent.com (orchd's --boot-set-issuer).
	BootSetIssuer string `yaml:"bootSetIssuer,omitempty"`
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

// Proxy is spec.proxy: the naru-proxy this Node runs, as user wire.ProxyUID. Its private key is
// the spec.files entry at ProxyKeyFile.
type Proxy struct {
	// Listen are the addresses the proxy terminates TLS on. It is ["0.0.0.0:443"] when empty.
	Listen []string `yaml:"listen,omitempty"`
	// Certificate is the PEM of the zone's wildcard certificate, then its chain.
	Certificate string `yaml:"certificate"`
	// RouteMaxAge is how long after hostd took a table the proxy forwards on it (a Go duration);
	// the proxy's own 1h when empty.
	RouteMaxAge string `yaml:"routeMaxAge,omitempty"`
}

func (p Proxy) listen() []string {
	if len(p.Listen) == 0 {
		return []string{"0.0.0.0:" + wire.ProxyPort}
	}
	return p.Listen
}

// tlsPort is the port of the first listen address on 0.0.0.0, which a proxy exposure's port 443
// is redirected to on the host (ADR 0112 item 8): the redirect sends a flow to the address of the
// interface it came in on, which only a wildcard listener is sure to have. It is 0 when there is
// none, which Parse refuses.
func (p Proxy) tlsPort() uint16 {
	for _, l := range p.listen() {
		if ap, err := netip.ParseAddrPort(l); err == nil && ap.Addr() == netip.IPv4Unspecified() {
			return ap.Port()
		}
	}
	return 0
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

func (o Orch) machineAPIListen() string {
	if o.MachineAPIListen == "" {
		return "0.0.0.0:" + wire.MachineAPIPort
	}
	return o.MachineAPIListen
}

func (o Orch) state() string {
	if o.Join != "" {
		return "existing"
	}
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

// orchCluster checks the initial cluster of a member that starts or joins by it.
func (v *validation) orchCluster(o *Orch, name string) {
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
}

// orchJoin checks a member that joins a running one, which hands it the initial cluster.
func (v *validation) orchJoin(o *Orch) {
	if !httpsURL(o.Join) {
		v.fail("spec.orch.join", "must be the https URL, with a host and a port, of a running member's client, got %q", o.Join)
	}
	if o.Restore {
		v.fail("spec.orch.join", "excludes spec.orch.restore: a member either restores a snapshot or joins a cluster")
	}
	if len(o.InitialCluster) != 0 {
		v.fail("spec.orch.initialCluster", "must be empty with spec.orch.join: the member takes the initial cluster from the one it joins")
	}
	if s := o.InitialClusterState; s != "" && s != "existing" {
		v.fail("spec.orch.initialClusterState", `must be "existing" with spec.orch.join, got %q`, s)
	}
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
	if o.Join != "" {
		v.orchJoin(o)
	} else {
		v.orchCluster(o, name)
	}
	for _, d := range []struct{ field, value string }{{"spec.orch.departAfter", o.DepartAfter}, {"spec.orch.housekeepingInterval", o.HousekeepingInterval}, {"spec.orch.etcdCompactionRetention", o.EtcdCompactionRetention}} {
		if d.value == "" {
			continue
		}
		if p, err := time.ParseDuration(d.value); err != nil || p <= 0 {
			v.fail(d.field, "must be a positive duration such as 30m, got %q", d.value)
		}
	}
	if o.EtcdQuotaBytes < 0 {
		v.fail("spec.orch.etcdQuotaBytes", "must not be negative, got %d", o.EtcdQuotaBytes)
	}
	if o.BootSetStore != "" {
		if strings.Contains(o.BootSetStore, "://") && !strings.HasPrefix(o.BootSetStore, "https://") {
			v.fail("spec.orch.bootSetStore", "must be an https URL or a directory path, got %q", o.BootSetStore)
		} else if strings.HasPrefix(o.BootSetStore, "https://") {
			u, err := url.Parse(o.BootSetStore)
			if err != nil || u.Hostname() == "" {
				v.fail("spec.orch.bootSetStore", "must be an https URL with a host, got %q", o.BootSetStore)
			}
		}
	}
	if o.BootSetTrustRoot != "" && !json.Valid([]byte(o.BootSetTrustRoot)) {
		v.fail("spec.orch.bootSetTrustRoot", "must be valid JSON")
	}
	if o.BootSetIdentity != "" {
		p, isPrefix := strings.CutSuffix(o.BootSetIdentity, "*")
		if strings.Contains(p, "*") {
			v.fail("spec.orch.bootSetIdentity", "may have a * only at its end")
		} else if isPrefix {
			after, ok := strings.CutPrefix(p, "https://github.com/")
			parts := strings.SplitN(after, "/", 3)
			if !ok || len(parts) < 3 || parts[0] == "" || parts[1] == "" {
				v.fail("spec.orch.bootSetIdentity", "prefix must start with https://github.com/<owner>/<repo>/ (the trailing slash is required)")
			}
		}
	}
	if o.BootSetIssuer != "" {
		u, err := url.Parse(o.BootSetIssuer)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			v.fail("spec.orch.bootSetIssuer", "must be an https URL, got %q", o.BootSetIssuer)
		}
	}
	if _, err := netip.ParseAddrPort(o.listen()); err != nil {
		v.fail("spec.orch.listen", "must be an IP address and a port, got %q", o.Listen)
	}
	if _, err := netip.ParseAddrPort(o.machineAPIListen()); err != nil {
		v.fail("spec.orch.machineApiListen", "must be an IP address and a port, got %q", o.MachineAPIListen)
	}
	if o.MachineZone != "" && !dnsSubdomain.MatchString(o.MachineZone) {
		v.fail("spec.orch.machineZone", "must be a lowercase DNS name, got %q", o.MachineZone)
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
	if o.HostCA != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(o.HostCA)) {
		v.fail("spec.orch.hostCA", "must be the PEM of the certificate authority the hosts' API certificates chain to")
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

func (v *validation) proxy(spec Spec) {
	p := spec.Proxy
	if p == nil {
		return
	}
	for i, l := range p.listen() {
		ap, err := netip.ParseAddrPort(l)
		switch {
		case err != nil:
			v.fail(fmt.Sprintf("spec.proxy.listen[%d]", i), "must be an IP address and a port, got %q", l)
		case ap.Port() == wire.ProxyRedirectPort:
			v.fail(fmt.Sprintf("spec.proxy.listen[%d]", i), "must not be port %d, where the proxy answers plain HTTP with a redirect to https", wire.ProxyRedirectPort)
		}
	}
	if p.tlsPort() == 0 {
		v.fail("spec.proxy.listen", "must have an address on 0.0.0.0, which a proxy exposure's port 443 is redirected to")
	}
	if !pemOf(p.Certificate, "CERTIFICATE") {
		v.fail("spec.proxy.certificate", "must be the PEM of the zone's certificate, then its chain")
	}
	if p.RouteMaxAge != "" {
		if d, err := time.ParseDuration(p.RouteMaxAge); err != nil || d <= 0 {
			v.fail("spec.proxy.routeMaxAge", "must be a positive duration such as 1h, got %q", p.RouteMaxAge)
		}
	}
	if !slices.ContainsFunc(spec.Services, func(s Service) bool { return s.Name == HostdService }) {
		v.fail("spec.proxy", "needs the %s service in spec.services: hostd writes the routes the proxy reads", HostdService)
	}
	i := slices.IndexFunc(spec.Files, func(f File) bool { return f.Path == ProxyKeyFile })
	switch {
	case i < 0:
		v.fail("spec.proxy", "needs a spec.files entry for %s, the zone's private key, owned by uid %d, gid %d, mode 0600", ProxyKeyFile, wire.ProxyUID, wire.ProxyUID)
	case spec.Files[i].Perm()&0o077 != 0 || spec.Files[i].Perm()&0o400 == 0:
		v.fail(fmt.Sprintf("spec.files[%d].mode", i), "must give the owner read and nobody else any access: it is the zone's private key")
	case spec.Files[i].Owner == nil || spec.Files[i].Owner.UID != wire.ProxyUID:
		v.fail(fmt.Sprintf("spec.files[%d].owner", i), "must be uid %d: the proxy runs as it and reads this file", wire.ProxyUID)
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
var hostdOwned = []string{"management-prefix", "egress-interface", "tunnel-port", "metrics-addr", "orch-url", "orch-ca-sha256", "enroll-key-file", "revocation-public-key", "revocation-max-age", "revocation-signer-socket", "revocation-seed", "proxy-health-addr", "proxy-tls-port"}

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
	dirs := []string{OrchRunDir + "/", SignerRunDir + "/", wire.ProxyRunDir + "/"}
	spec.Files = slices.DeleteFunc(slices.Clone(spec.Files), func(f File) bool {
		return f.Path == HostdEnrollKeyFile || f.Path == HostdRevocationKeyFile || f.Path == HostdRevocationSeedFile || f.Path != OrchKeyFile && f.Path != ProxyKeyFile && slices.ContainsFunc(dirs, func(d string) bool { return strings.HasPrefix(f.Path, d) })
	})
	var services []Service
	for _, s := range spec.Services {
		switch s.Name {
		case SignerService, OrchService, ProxyService:
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
			if h := spec.Host; h != nil && h.RevocationPublicKey != "" {
				args, files := hostdRevocation(spec)
				s.Args = append(s.Args, args...)
				spec.Files = append(spec.Files, files...)
			}
			if spec.Metrics != nil {
				s.Args = append(s.Args, "--metrics-addr="+net.JoinHostPort(spec.Metrics.Listen, strconv.Itoa(HostdMetricsPort)))
			}
			if p := spec.Proxy; p != nil {
				s.Args = append(s.Args, "--proxy-health-addr="+wire.ProxyHealthAddr, "--proxy-tls-port="+strconv.Itoa(int(p.tlsPort())))
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
	if p := spec.Proxy; p != nil {
		svc, file := proxyService(*p)
		services = append(services, svc)
		spec.Files = append(spec.Files, file)
	}
	spec.Services = services
	cfg.Spec = spec
	return cfg
}

// proxyOpenFiles is naru-proxy's limit of open files: two for each of the connections it carries
// at once, at most wire.ProxyMaxConns, and room for its listeners and files.
const proxyOpenFiles = 2*wire.ProxyMaxConns + 1024

// proxyService runs naru-proxy as wire.ProxyUID, able to bind port 443 and to hold a descriptor for
// each side of every connection it carries, with its own directory
// for the revision file and the directory hostd writes the routes to in its group.
func proxyService(p Proxy) (Service, File) {
	args := []string{
		"--listen=" + strings.Join(p.listen(), ","), "--cert=" + ProxyCertFile, "--key=" + ProxyKeyFile,
		"--routes-file=" + wire.ProxyRoutesFile, "--revision-file=" + wire.ProxyRevisionFile, "--health-addr=" + wire.ProxyHealthAddr,
		"--redirect-listen=" + wire.ProxyRedirectListen,
	}
	if p.RouteMaxAge != "" {
		args = append(args, "--route-max-age="+p.RouteMaxAge)
	}
	return Service{
		Name: ProxyService, Binary: proxyBinary, Args: args,
		User:         &User{UID: wire.ProxyUID, GID: wire.ProxyUID},
		BindLowPorts: true,
		OpenFiles:    proxyOpenFiles,
		Dirs: []Dir{
			{Path: wire.ProxyRunDir, Mode: "0755", UID: wire.ProxyUID, GID: wire.ProxyUID},
			{Path: wire.ProxyRoutesDir, Mode: "0750", UID: 0, GID: wire.ProxyUID},
		},
		RestartOnChange: []string{ProxyKeyFile},
	}, File{Path: ProxyCertFile, Mode: "0644", Content: p.Certificate, Restart: []string{ProxyService}}
}

// hostdRevocation returns the flags and files that give naru-hostd the revocation list's key, max
// age, source and the list delivered with the configuration.
func hostdRevocation(spec Spec) ([]string, []File) {
	h := spec.Host
	args := []string{"--revocation-public-key=" + HostdRevocationKeyFile}
	files := []File{{Path: HostdRevocationKeyFile, Mode: "0644", Content: h.RevocationPublicKey, Restart: []string{HostdService}}}
	if age, err := RevocationMaxAgeOf(h.RevocationMaxAge); err == nil && h.RevocationMaxAge != "" {
		args = append(args, "--revocation-max-age="+age.String())
	}
	if from, ok := spec.RevocationFrom(); ok && from.Socket != "" {
		args = append(args, "--revocation-signer-socket="+from.Socket)
	}
	if h.RevocationList != "" {
		args = append(args, "--revocation-seed="+HostdRevocationSeedFile)
		files = append(files, File{Path: HostdRevocationSeedFile, Mode: "0644", Content: h.RevocationList, Restart: []string{HostdService}})
	}
	return args, files
}

func metricsFlag(m *Metrics, port int) []string {
	if m == nil {
		return nil
	}
	return []string{"--metrics-addr=" + net.JoinHostPort(m.Listen, strconv.Itoa(port))}
}

func signerService(s SignerSection, node string, m *Metrics) (Service, []File) {
	user := &User{UID: SignerUID, GID: SignerUID}
	args := []string{"serve", "--dir=" + SignerDir, "--socket=" + SignerSocket, "--socket-mode=0660"}
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
	cluster := "--initial-cluster=" + strings.Join(members, ",")
	if o.Join != "" {
		cluster = "--join=" + o.Join
	}
	args := []string{
		"--name=" + o.name(node), "--data-dir=" + OrchDataDir + "/etcd", "--peer-url=" + o.PeerURL, "--client-url=" + o.ClientURL,
		cluster, "--initial-cluster-state=" + o.state(), "--listen=" + o.listen(),
		"--cert=" + OrchRunDir + "/orch.crt", "--key=" + OrchKeyFile, "--ca=" + OrchRunDir + "/ca.crt",
		"--signer-socket=" + SignerSocket, "--snapshot-dir=" + OrchDataDir + "/snapshots",
		"--revocation-public-key=" + OrchRunDir + "/revocation.pub",
		"--host-client-cert=" + OrchRunDir + "/orch.crt", "--host-client-key=" + OrchKeyFile,
	}
	if o.MachineZone != "" {
		args = append(args, "--machine-api-addr="+o.machineAPIListen(), "--machine-zone="+o.MachineZone)
	}
	if o.Restore {
		args = append(args, "--restore")
	}
	if o.DepartAfter != "" {
		args = append(args, "--depart-after="+o.DepartAfter)
	}
	if o.HousekeepingInterval != "" {
		args = append(args, "--housekeeping-interval="+o.HousekeepingInterval)
	}
	if o.EtcdQuotaBytes > 0 {
		args = append(args, "--etcd-quota-bytes="+strconv.FormatInt(o.EtcdQuotaBytes, 10))
	}
	if o.EtcdCompactionRetention != "" {
		args = append(args, "--etcd-compaction-retention="+o.EtcdCompactionRetention)
	}
	restart := []string{OrchService}
	files := []File{
		{Path: OrchRunDir + "/orch.crt", Mode: "0644", Content: o.Certificate, Restart: restart},
		{Path: OrchRunDir + "/ca.crt", Mode: "0644", Content: o.CA, Restart: restart},
		{Path: OrchRunDir + "/revocation.pub", Mode: "0644", Content: o.RevocationPublicKey, Restart: restart},
	}
	if o.HostCA != "" {
		args = append(args, "--host-ca="+OrchHostCAFile)
		files = append(files, File{Path: OrchHostCAFile, Mode: "0644", Content: o.HostCA, Restart: restart})
	}
	if o.RecoveryPublicKey != "" {
		args = append(args, "--recovery-public-key="+OrchRunDir+"/recovery.pub")
		files = append(files, File{Path: OrchRunDir + "/recovery.pub", Mode: "0644", Content: o.RecoveryPublicKey, Restart: restart})
	}
	if o.BootSetStore != "" {
		args = append(args, "--boot-set-store="+o.BootSetStore)
	}
	if o.BootSetTrustRoot != "" {
		args = append(args, "--boot-set-trust-root="+OrchBootSetTrustRootFile)
		files = append(files, File{Path: OrchBootSetTrustRootFile, Mode: "0644", Content: o.BootSetTrustRoot, Restart: restart})
	}
	if o.BootSetIdentity != "" {
		args = append(args, "--boot-set-identity="+o.BootSetIdentity)
	}
	if o.BootSetIssuer != "" {
		args = append(args, "--boot-set-issuer="+o.BootSetIssuer)
	}
	return Service{
		Name: OrchService, Binary: orchBinary, Args: append(args, metricsFlag(m, OrchMetricsPort)...),
		User:            &User{UID: OrchUID, GID: OrchUID, Groups: []uint32{SignerUID}},
		Dirs:            []Dir{{Path: OrchDataDir, Mode: "0700", UID: OrchUID, GID: OrchUID}},
		RestartOnChange: []string{OrchKeyFile},
	}, files
}
