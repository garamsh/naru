// Package orchd composes one naru-orchd member: its etcd member, leader election, inventory, API
// and snapshots.
package orchd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/garamsh/naru/orch/internal/api"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/inventory"
	invetcd "github.com/garamsh/naru/orch/internal/inventory/etcd"
	"github.com/garamsh/naru/orch/internal/leader"
	"github.com/garamsh/naru/orch/internal/member"
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
	Limits           inventory.Config
}

// Deps are the parts the binary's main chooses.
type Deps struct {
	Signer     inventory.Signer
	Auth       auth.Authenticator
	Sealer     snapshot.Sealer
	Reconciler Reconciler
	Clock      inventory.Clock
	Log        *slog.Logger
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
	svc := inventory.NewService(invetcd.New(m.Client()), d.Signer, d.Clock, c.Limits)
	handler := api.New(api.Deps{Inventory: svc, Signer: d.Signer, Auth: d.Auth, Snapshots: taker, Log: d.Log})

	o.wg.Add(3)
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
		leader.Run(runCtx, m.Client(), c.Name, c.LeaderTTL, d.Log, o.lead(d))
	}()
	return o, nil
}

// lead is what the leader does: note itself, then run the reconciler until leadership ends.
func (o *Orchd) lead(d Deps) func(context.Context) {
	return func(ctx context.Context) {
		o.leading.Store(true)
		defer o.leading.Store(false)
		d.Log.Info("leading", "member", o.name)
		if d.Reconciler == nil {
			<-ctx.Done()
			return
		}
		for ctx.Err() == nil {
			if err := d.Reconciler.Run(ctx); err != nil && ctx.Err() == nil {
				d.Log.Error("reconciler failed", "err", err.Error())
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
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
