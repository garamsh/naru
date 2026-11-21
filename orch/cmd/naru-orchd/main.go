// Command naru-orchd is one member of orch (docs/architecture/orch.md): an embedded etcd, one
// elected leader, and the HTTPS API that enrolls and admits hosts. It is the composition site.
package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/crlsync"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/groups/release"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/netrec"
	"github.com/garamsh/naru/orch/internal/orchd"
	"github.com/garamsh/naru/orch/internal/signer"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func main() {
	var (
		name        = flag.String("name", "", "this member's name in the etcd cluster")
		dataDir     = flag.String("data-dir", "/var/lib/naru/orch/etcd", "etcd's data directory")
		peerURL     = flag.String("peer-url", "", "https URL this member's etcd listens on for its peers")
		clientURL   = flag.String("client-url", "", "https URL this member's etcd serves clients on")
		initial     = flag.String("initial-cluster", "", "name=peer-url,... of the members of a new cluster")
		state       = flag.String("initial-cluster-state", member.StateNew, "new, or existing when this member joins a cluster")
		join        = flag.String("join", "", "client URL of a running member: with --initial-cluster-state existing, this member adds itself to that cluster and takes --initial-cluster from it")
		restore     = flag.Bool("restore", false, "with no etcd member in --data-dir, serve only PUT /v1/restore and start etcd on the snapshot it receives")
		listen      = flag.String("listen", "0.0.0.0:7500", "address of the HTTPS API")
		certFile    = flag.String("cert", "/var/lib/naru/orch/tls/orch.crt", "orch's certificate, followed by the authority's")
		keyFile     = flag.String("key", "/var/lib/naru/orch/tls/orch.key", "orch's key")
		caFile      = flag.String("ca", "/var/lib/naru/orch/tls/ca.crt", "the authority that issues every identity")
		sock        = flag.String("signer-socket", "/run/naru-signer/signer.sock", "naru-signer's unix socket")
		snapDir     = flag.String("snapshot-dir", "/var/lib/naru/orch/snapshots", "where sealed snapshots are kept")
		revoke      = flag.String("revocation-public-key", "/var/lib/naru/orch/tls/revocation.pub", "the Ed25519 public key (PKIX, PEM) that verifies the revocation list")
		recovery    = flag.String("recovery-public-key", "", "the recovery key's public half; without it no snapshot is written")
		snapEvery   = flag.Duration("snapshot-interval", snapshot.DefaultInterval, "how often a snapshot is taken")
		maxAge      = flag.Duration("revocation-max-age", identity.DefaultMaxAge, "how long a revocation list that cannot be refreshed is used before callers are refused with 503")
		crlEvery    = flag.Duration("revocation-sync-interval", crlsync.DefaultInterval, "how often the revocation list is synced between the signers")
		snapKeep    = flag.Int("snapshot-keep", snapshot.DefaultKeep, "how many snapshots are kept")
		hostCert    = flag.String("host-client-cert", "", "the certificate orch presents to the hosts' APIs; defaults to --cert")
		hostKey     = flag.String("host-client-key", "", "the key of --host-client-cert; defaults to --key")
		hostCA      = flag.String("host-ca", "", "the authority that issued the hosts' API certificates; defaults to --ca")
		groupEach   = flag.Duration("group-interval", groups.DefaultInterval, "the pause between the groups engine's passes")
		netEach     = flag.Duration("network-interval", netrec.DefaultInterval, "the pause between the network reconciler's passes")
		departAfter = flag.Duration("depart-after", member.DepartAfter, "how long the leader sees a member unreachable before it removes the member so that it rejoins empty")
		houseEvery  = flag.Duration("housekeeping-interval", housekeepEvery, "the pause between the leader's etcd housekeeping passes")
		etcdQuota   = flag.Int64("etcd-quota-bytes", 0, "the etcd backend quota; 0 derives it from the size of the data disk")
		etcdKeep    = flag.String("etcd-compaction-retention", "", "how much history etcd keeps, as a duration; empty keeps "+member.CompactionRetention)
		bsStore     = flag.String("boot-set-store", "", "https URL or directory of the release assets the Node boot sets are added from; empty adds none")
		bsRoot      = flag.String("boot-set-trust-root", "", "the Sigstore trusted_root.json that verifies a release; defaults to the public-good root built in")
		bsIdentity  = flag.String("boot-set-identity", "", "the certificate identity a release is signed by; a trailing * is a prefix of at least https://github.com/<owner>/<repo>/; defaults to the release workflow of garamsh/naru at the version's tag")
		bsIssuer    = flag.String("boot-set-issuer", release.DefaultIssuer, "the OIDC issuer of the certificate a release is signed with")
		metricsAddr = flag.String("metrics-addr", "", "address that serves /metrics over plain HTTP; empty serves none")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if err := run(log, runFlags{
		name: *name, dataDir: *dataDir, peerURL: *peerURL, clientURL: *clientURL, initial: *initial, state: *state, join: *join, restore: *restore,
		listen: *listen, cert: *certFile, key: *keyFile, ca: *caFile, sock: *sock,
		snapDir: *snapDir, revoke: *revoke, recovery: *recovery, snapEvery: *snapEvery, snapKeep: *snapKeep, maxAge: *maxAge, crlEvery: *crlEvery,
		hostCert: *hostCert, hostKey: *hostKey, hostCA: *hostCA, groupEach: *groupEach, netEach: *netEach, departAfter: *departAfter, houseEvery: *houseEvery, quota: *etcdQuota, keep: *etcdKeep, metricsAddr: *metricsAddr,
		bootSets: release.Config{Location: *bsStore, TrustRoot: *bsRoot, Identity: *bsIdentity, Issuer: *bsIssuer},
	}); err != nil {
		log.Error("naru-orchd stopped", "err", err.Error())
		os.Exit(1)
	}
}

type runFlags struct {
	name, dataDir, peerURL, clientURL, initial, state, listen string
	cert, key, ca, sock, snapDir, revoke, recovery            string
	snapEvery, maxAge, crlEvery, groupEach, netEach           time.Duration
	departAfter, houseEvery                                   time.Duration
	quota                                                     int64
	keep                                                      string
	hostCert, hostKey, hostCA                                 string
	snapKeep                                                  int
	metricsAddr                                               string
	join                                                      string
	restore                                                   bool
	bootSets                                                  release.Config
}

func run(log *slog.Logger, f runFlags) error {
	required := map[string]string{"name": f.name, "peer-url": f.peerURL, "client-url": f.clientURL}
	if f.join == "" {
		required["initial-cluster"] = f.initial
	}
	for flagName, v := range required {
		if v == "" {
			return fmt.Errorf("--%s is required", flagName)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	sg := signer.New(f.sock)
	revokeKey, err := readRevocationKey(f.revoke)
	if err != nil {
		return err
	}
	revocations := newRevocations(sg, revokeKey, f.maxAge, log)
	authn := auth.Revocations{List: revocations}
	sealer, err := newSealer(f.recovery)
	if err != nil {
		return err
	}
	hostHTTP, err := newHostHTTP(cmp.Or(f.hostCert, f.cert), cmp.Or(f.hostKey, f.key), cmp.Or(f.hostCA, f.ca))
	if err != nil {
		return err
	}
	bootSets, err := newBootSets(f.bootSets)
	if err != nil {
		return err
	}
	var reg *prometheus.Registry
	var registerer prometheus.Registerer
	if f.metricsAddr != "" {
		reg = newRegistry(f.name)
		registerer = reg
	}
	mcfg := member.Config{
		Name: f.name, DataDir: f.dataDir, PeerURL: f.peerURL, ClientURL: f.clientURL,
		InitialCluster: f.initial, State: f.state, Join: f.join, Cert: f.cert, Key: f.key, CA: f.ca, Log: log,
		QuotaBytes: f.quota, CompactionRetention: f.keep,
	}
	quota, err := mcfg.BackendQuota()
	if err != nil {
		return err
	}
	o, err := orchd.Start(ctx, orchd.Config{
		Config: mcfg,
		Listen: f.listen, ClientCA: f.ca, SnapshotDir: f.snapDir, SnapshotInterval: f.snapEvery, SnapshotKeep: f.snapKeep, CRLSyncInterval: f.crlEvery,
		GroupInterval: f.groupEach, NetworkInterval: f.netEach, Restore: f.restore,
	}, orchd.Deps{
		Signer: sg, Auth: authn, Sealer: sealer, Clock: realClock{}, Log: log, CRLPeer: sg, RevocationKey: revokeKey, Metrics: registerer,
		NewHosts:        func(inv inventory.Service) groups.Hosts { return hostclient.New(inv, hostHTTP, revocations) },
		BootSets:        bootSets,
		NewBootSetHosts: func(inv inventory.Service) groups.BootSetHosts { return hostclient.New(inv, hostHTTP, revocations) },
		NewNetworkHosts: func(inv inventory.Service) netrec.Hosts {
			return hostclient.NewNetHosts(inv, hostclient.DialNetAPI(hostHTTP, revocations))
		},
		NewExposureHosts: func(inv inventory.Service) orchd.ExposureHosts {
			return hostclient.NewExposureHosts(inv, hostclient.DialExposureAPI(hostHTTP, revocations))
		},
	})
	if err != nil {
		return err
	}
	go housekeep(ctx, log, o, quota, f.departAfter, f.houseEvery)
	if reg != nil {
		go serveMetrics(ctx, log, f.metricsAddr, reg)
	}
	log.Info("naru-orchd serving", "member", f.name, "api", o.Addr())
	<-ctx.Done()
	return o.Stop()
}

// newBootSets is the store the boot sets are added from, or nil when none is set. A store whose
// identity or trust root cannot be built is an error at start and not a store that trusts its releases.
func newBootSets(c release.Config) (groups.ReleaseStore, error) {
	if c.Location == "" {
		return nil, nil
	}
	return release.New(c)
}
