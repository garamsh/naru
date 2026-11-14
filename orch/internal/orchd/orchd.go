// Package orchd composes one naru-orchd member: its etcd member, leader election, inventory, groups
// and networks APIs, the groups engine and the network reconciler, and snapshots.
package orchd

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/orch/internal/api"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/crlsync"
	"github.com/garamsh/naru/orch/internal/groups"
	groupsetcd "github.com/garamsh/naru/orch/internal/groups/etcd"
	"github.com/garamsh/naru/orch/internal/groupsvc"
	"github.com/garamsh/naru/orch/internal/inventory"
	invetcd "github.com/garamsh/naru/orch/internal/inventory/etcd"
	"github.com/garamsh/naru/orch/internal/leader"
	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/netrec"
	"github.com/garamsh/naru/orch/internal/networks"
	netetcd "github.com/garamsh/naru/orch/internal/networks/etcd"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

// DefaultLeaderTTL is the lease of the leader: a stopped leader is replaced within about this long.
const DefaultLeaderTTL = 10 * time.Second

// Config is one member's settings.
type Config struct {
	member.Config
	// Listen is the API's address.
	Listen string
	// ClientCA verifies the certificates of the API's callers.
	ClientCA string
	// SnapshotDir holds the sealed snapshots; SnapshotInterval and SnapshotKeep default to every
	// 6 hours and 8 files.
	SnapshotDir      string
	SnapshotInterval time.Duration
	SnapshotKeep     int
	LeaderTTL        time.Duration
	// CRLSyncInterval is how often the member syncs the revocation list; it defaults to
	// crlsync.DefaultInterval.
	CRLSyncInterval time.Duration
	// GroupInterval is the pause between the groups engine's passes; it defaults to groups.DefaultInterval.
	GroupInterval time.Duration
	// NetworkInterval is the pause between the network reconciler's passes; it defaults to
	// netrec.DefaultInterval.
	NetworkInterval time.Duration
	Limits          inventory.Config
}

// Deps are the parts the binary's main chooses.
type Deps struct {
	Signer inventory.Signer
	Auth   auth.Authenticator
	Sealer snapshot.Sealer
	// NewHosts returns the engine's view of the hosts, given the inventory it reads them from. With
	// it the leader runs the groups engine; without it, and without a Reconciler, no group is
	// reconciled.
	NewHosts func(inv inventory.Service) groups.Hosts
	// NewNetworkHosts returns the network reconciler's view of the hosts. With it the leader runs
	// the network reconciler beside the groups engine; without it no host is told its networks.
	NewNetworkHosts func(inv inventory.Service) netrec.Hosts
	// Reconciler replaces the groups engine, for a test of the leader seam.
	Reconciler Reconciler
	Clock      inventory.Clock
	Log        *slog.Logger
	// CRLPeer, with RevocationKey, turns on the sync of the revocation list between the signers: it
	// is the local signer. Without it the list is not synced.
	CRLPeer       crlsync.Peer
	RevocationKey ed25519.PublicKey
}

// Orchd is a running member.
type Orchd struct {
	name    string
	member  *member.Member
	addr    string
	leading atomic.Bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stop    sync.Once
	log     *slog.Logger
}

// Start runs the member and returns once its etcd has a quorum and its API listens.
func Start(ctx context.Context, c Config, d Deps) (*Orchd, error) {
	if c.LeaderTTL == 0 {
		c.LeaderTTL = DefaultLeaderTTL
	}
	if c.SnapshotInterval == 0 {
		c.SnapshotInterval = snapshot.DefaultInterval
	}
	tlsConf, err := serverTLS(c)
	if err != nil {
		return nil, err
	}
	m, err := member.Start(ctx, c.Config)
	if err != nil {
		return nil, err
	}
	l, err := tls.Listen("tcp", c.Listen, tlsConf)
	if err != nil {
		m.Close()
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	o := &Orchd{name: c.Name, member: m, addr: l.Addr().String(), cancel: cancel, log: d.Log}

	taker := &snapshot.Taker{Source: snapshot.ClientSource(m.Client()), Sealer: d.Sealer, Dir: c.SnapshotDir, Keep: c.SnapshotKeep, Now: d.Clock.Now, Log: d.Log}
	invStore := invetcd.New(m.Client())
	svc := inventory.NewService(invStore, d.Signer, d.Clock, c.Limits)
	groupStore := groupsetcd.New(m.Client())
	var hosts groups.Hosts
	if d.NewHosts != nil {
		hosts = d.NewHosts(svc)
	}
	if d.Reconciler == nil && hosts != nil {
		d.Reconciler = groups.New(groups.Config{
			Hosts: hosts, Store: groupStore, Clock: groupClock{d.Clock}, Events: groupsetcd.NewEvents(invStore), Interval: c.GroupInterval,
		})
	}
	networkStore := netetcd.New(m.Client())
	var reconcilers []Reconciler
	if d.Reconciler != nil {
		reconcilers = append(reconcilers, d.Reconciler)
	}
	if d.NewNetworkHosts != nil {
		reconcilers = append(reconcilers, netrec.New(netrec.Config{Hosts: d.NewNetworkHosts(svc), Networks: networkStore, Interval: c.NetworkInterval, Log: d.Log}))
	}
	handler := api.New(api.Deps{Inventory: svc, Groups: groupsvc.New(groupStore, hosts, invStore, d.Clock), Networks: networks.New(networkStore, groupStore, invStore, d.Clock), Signer: d.Signer, Auth: d.Auth, Snapshots: taker, Log: d.Log})

	o.wg.Add(4)
	go func() {
		defer o.wg.Done()
		if err := api.Serve(runCtx, l, handler); err != nil {
			d.Log.Error("api stopped", "err", err.Error())
		}
	}()
	go func() {
		defer o.wg.Done()
		if d.Sealer == nil {
			d.Log.Warn("no recovery public key: scheduled snapshots are off")
			return
		}
		taker.Run(runCtx, c.SnapshotInterval)
	}()
	go func() {
		defer o.wg.Done()
		if d.CRLPeer == nil {
			d.Log.Warn("no revocation list peer: the signers' lists are not synced")
			return
		}
		(&crlsync.Syncer{Store: crlsync.EtcdStore{Client: m.Client()}, Peer: d.CRLPeer, Key: d.RevocationKey, Interval: c.CRLSyncInterval, Log: d.Log}).Run(runCtx)
	}()
	go func() {
		defer o.wg.Done()
		leader.Run(runCtx, m.Client(), c.Name, c.LeaderTTL, d.Log, o.lead(d, reconcilers))
	}()
	return o, nil
}

// groupClock gives the engine the deployment's clock and real waits.
type groupClock struct{ inventory.Clock }

func (groupClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// lead is what the leader does: note itself, then run the reconcilers until leadership ends.
func (o *Orchd) lead(d Deps, reconcilers []Reconciler) func(context.Context) {
	return func(ctx context.Context) {
		o.leading.Store(true)
		defer o.leading.Store(false)
		d.Log.Info("leading", "member", o.name)
		var wg sync.WaitGroup
		for _, r := range reconcilers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o.keepRunning(ctx, d, r)
			}()
		}
		wg.Wait()
		<-ctx.Done()
	}
}

// keepRunning runs the reconciler again after a failure, while the member still leads.
func (o *Orchd) keepRunning(ctx context.Context, d Deps, r Reconciler) {
	for ctx.Err() == nil {
		if err := r.Run(ctx); err != nil && ctx.Err() == nil {
			d.Log.Error("reconciler failed", "err", err.Error())
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func serverTLS(c Config) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, err
	}
	pem, err := os.ReadFile(c.ClientCA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificate", c.ClientCA)
	}
	return api.TLSConfig(pair, pool), nil
}

// Addr is the address the API listens on.
func (o *Orchd) Addr() string { return o.addr }

// Etcd is the member's etcd client, for a test that reads what orch stored.
func (o *Orchd) Etcd() *clientv3.Client { return o.member.Client() }

// IsLeader reports whether this member currently leads.
func (o *Orchd) IsLeader() bool { return o.leading.Load() }

// Leader returns the name of the member that leads, as etcd records it.
func (o *Orchd) Leader(ctx context.Context) (string, error) {
	return leader.Current(ctx, o.member.Client())
}

// Stop ends the member and waits for its goroutines. It resigns nothing: the lease lapses.
func (o *Orchd) Stop() error {
	o.stop.Do(func() {
		o.cancel()
		o.wg.Wait()
		o.member.Close()
	})
	return nil
}

// Name is the member's name.
func (o *Orchd) Name() string { return o.name }
