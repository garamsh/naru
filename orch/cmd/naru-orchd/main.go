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
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/crlsync"
	"github.com/garamsh/naru/orch/internal/groups"
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
		name      = flag.String("name", "", "this member's name in the etcd cluster")
		dataDir   = flag.String("data-dir", "/var/lib/naru/orch/etcd", "etcd's data directory")
		peerURL   = flag.String("peer-url", "", "https URL this member's etcd listens on for its peers")
		clientURL = flag.String("client-url", "", "https URL this member's etcd serves clients on")
		initial   = flag.String("initial-cluster", "", "name=peer-url,... of the members of a new cluster")
		state     = flag.String("initial-cluster-state", member.StateNew, "new, or existing when this member joins a cluster")
		listen    = flag.String("listen", "0.0.0.0:7500", "address of the HTTPS API")
		certFile  = flag.String("cert", "/var/lib/naru/orch/tls/orch.crt", "orch's certificate, followed by the authority's")
		keyFile   = flag.String("key", "/var/lib/naru/orch/tls/orch.key", "orch's key")
		caFile    = flag.String("ca", "/var/lib/naru/orch/tls/ca.crt", "the authority that issues every identity")
		sans      = flag.String("san", "127.0.0.1", "comma-separated names and addresses of this member, for its certificate")
		sock      = flag.String("signer-socket", "/run/naru-signer/signer.sock", "naru-signer's unix socket")
		snapDir   = flag.String("snapshot-dir", "/var/lib/naru/orch/snapshots", "where sealed snapshots are kept")
		revoke    = flag.String("revocation-public-key", "/var/lib/naru/orch/tls/revocation.pub", "the Ed25519 public key (PKIX, PEM) that verifies the revocation list")
		recovery  = flag.String("recovery-public-key", "", "the recovery key's public half; without it no snapshot is written")
		snapEvery = flag.Duration("snapshot-interval", snapshot.DefaultInterval, "how often a snapshot is taken")
		maxAge    = flag.Duration("revocation-max-age", auth.DefaultMaxAge, "how long a revocation list that cannot be refreshed is used before callers are refused with 503")
		crlEvery  = flag.Duration("revocation-sync-interval", crlsync.DefaultInterval, "how often the revocation list is synced between the signers")
		snapKeep  = flag.Int("snapshot-keep", snapshot.DefaultKeep, "how many snapshots are kept")
		hostCert  = flag.String("host-client-cert", "", "the certificate orch presents to the hosts' APIs; defaults to --cert")
		hostKey   = flag.String("host-client-key", "", "the key of --host-client-cert; defaults to --key")
		hostCA    = flag.String("host-ca", "", "the authority that issued the hosts' API certificates; defaults to --ca")
		groupEach = flag.Duration("group-interval", groups.DefaultInterval, "the pause between the groups engine's passes")
		netEach   = flag.Duration("network-interval", netrec.DefaultInterval, "the pause between the network reconciler's passes")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, runFlags{
		name: *name, dataDir: *dataDir, peerURL: *peerURL, clientURL: *clientURL, initial: *initial, state: *state,
		listen: *listen, cert: *certFile, key: *keyFile, ca: *caFile, sans: *sans, sock: *sock,
		snapDir: *snapDir, revoke: *revoke, recovery: *recovery, snapEvery: *snapEvery, snapKeep: *snapKeep, maxAge: *maxAge, crlEvery: *crlEvery,
		hostCert: *hostCert, hostKey: *hostKey, hostCA: *hostCA, groupEach: *groupEach, netEach: *netEach,
	}); err != nil {
		log.Error("naru-orchd stopped", "err", err.Error())
		os.Exit(1)
	}
}

type runFlags struct {
	name, dataDir, peerURL, clientURL, initial, state, listen string
	cert, key, ca, sans, sock, snapDir, revoke, recovery      string
	snapEvery, maxAge, crlEvery, groupEach, netEach           time.Duration
	hostCert, hostKey, hostCA                                 string
	snapKeep                                                  int
}

func run(log *slog.Logger, f runFlags) error {
	for flagName, v := range map[string]string{"name": f.name, "peer-url": f.peerURL, "client-url": f.clientURL, "initial-cluster": f.initial} {
		if v == "" {
			return fmt.Errorf("--%s is required", flagName)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	sg := signer.New(f.sock)
	if err := sg.EnsureIdentity(ctx, signer.Files{Cert: f.cert, Key: f.key, CA: f.ca}, f.name, strings.Split(f.sans, ",")); err != nil {
		return err
	}
	revokeKey, err := readRevocationKey(f.revoke)
	if err != nil {
		return err
	}
	authn := newAuthenticator(sg, revokeKey, f.maxAge, log)
	sealer, err := newSealer(f.recovery)
	if err != nil {
		return err
	}
	hostHTTP, err := newHostHTTP(cmp.Or(f.hostCert, f.cert), cmp.Or(f.hostKey, f.key), cmp.Or(f.hostCA, f.ca))
	if err != nil {
		return err
	}
	o, err := orchd.Start(ctx, orchd.Config{
		Config: member.Config{
			Name: f.name, DataDir: f.dataDir, PeerURL: f.peerURL, ClientURL: f.clientURL,
			InitialCluster: f.initial, State: f.state, Cert: f.cert, Key: f.key, CA: f.ca,
		},
		Listen: f.listen, ClientCA: f.ca, SnapshotDir: f.snapDir, SnapshotInterval: f.snapEvery, SnapshotKeep: f.snapKeep, CRLSyncInterval: f.crlEvery,
		GroupInterval: f.groupEach, NetworkInterval: f.netEach,
	}, orchd.Deps{
		Signer: sg, Auth: authn, Sealer: sealer, Clock: realClock{}, Log: log, CRLPeer: sg, RevocationKey: revokeKey,
		NewHosts: func(inv inventory.Service) groups.Hosts { return hostclient.New(inv, hostHTTP) },
		NewNetworkHosts: func(inv inventory.Service) netrec.Hosts {
			return hostclient.NewNetHosts(inv, hostclient.DialNetAPI(hostHTTP))
		},
	})
	if err != nil {
		return err
	}
	log.Info("naru-orchd serving", "member", f.name, "api", o.Addr())
	<-ctx.Done()
	return o.Stop()
}
