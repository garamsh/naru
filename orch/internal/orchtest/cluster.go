package orchtest

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/crlsync"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/orchd"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

// Cluster is n orch members on localhost sharing one authority.
type Cluster struct {
	CA      *CA
	Signer  *Signer
	Clock   *Clock
	Members []*orchd.Orchd
	Log     *slog.Logger
	dir     string
}

// Options tunes a cluster.
type Options struct {
	Members int
	Sealer  snapshot.Sealer
	Limits  inventory.Config
	// LeaderTTL defaults to 3 seconds, which keeps failover tests short.
	LeaderTTL time.Duration
	// NewHosts gives the groups engine its hosts; without it no group is reconciled.
	NewHosts func(inv inventory.Service) groups.Hosts
	// GroupInterval is the pause between the engine's passes; it defaults to 200 ms.
	GroupInterval time.Duration
	// Log receives the members' logs; it defaults to discarding them.
	Log *slog.Logger
	// CA, when set, is the authority the cluster uses; a test that builds peers first passes it.
	CA *CA
	// CRLPeers, when set, are the revocation list peers of the members, by index, and the members
	// sync the list every CRLSyncInterval.
	CRLPeers        []crlsync.Peer
	CRLSyncInterval time.Duration
}

func freePorts(t testing.TB, n int) []int {
	t.Helper()
	var ls []net.Listener
	var ports []int
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range ls {
		_ = l.Close()
	}
	return ports
}

// Start runs the cluster and returns once every member serves. Members stop when the test ends.
func Start(t testing.TB, o Options) *Cluster {
	t.Helper()
	if o.Members == 0 {
		o.Members = 3
	}
	if o.LeaderTTL == 0 {
		o.LeaderTTL = 3 * time.Second
	}
	if o.GroupInterval == 0 {
		o.GroupInterval = 200 * time.Millisecond
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ca := o.CA
	if ca == nil {
		ca = NewCA(t)
	}
	c := &Cluster{CA: ca, Signer: &Signer{CA: ca}, Clock: NewClock(), Log: o.Log, dir: t.TempDir()}

	ports := freePorts(t, 2*o.Members)
	var initial []string
	for i := range o.Members {
		initial = append(initial, fmt.Sprintf("m%d=https://127.0.0.1:%d", i, ports[2*i]))
	}
	type result struct {
		o   *orchd.Orchd
		err error
	}
	results := make(chan result, o.Members)
	for i := range o.Members {
		name := fmt.Sprintf("m%d", i)
		dir := filepath.Join(c.dir, name)
		files := ca.IssueFiles(t, c.dir, name)
		cfg := orchd.Config{
			Config: member.Config{
				Name: name, DataDir: filepath.Join(dir, "etcd"),
				PeerURL:        fmt.Sprintf("https://127.0.0.1:%d", ports[2*i]),
				ClientURL:      fmt.Sprintf("https://127.0.0.1:%d", ports[2*i+1]),
				InitialCluster: strings.Join(initial, ","), State: member.StateNew,
				Cert: files.Cert, Key: files.Key, CA: files.CA,
			},
			Listen: "127.0.0.1:0", ClientCA: files.CA, SnapshotDir: filepath.Join(dir, "snapshots"),
			LeaderTTL: o.LeaderTTL, Limits: o.Limits, CRLSyncInterval: o.CRLSyncInterval, GroupInterval: o.GroupInterval,
		}
		deps := orchd.Deps{Signer: c.Signer, Auth: &auth.Revocations{Source: c.Signer, Key: ca.RevocationKey(), Now: time.Now, Log: o.Log}, Sealer: o.Sealer, NewHosts: o.NewHosts, Clock: c.Clock, Log: o.Log.With("member", name)}
		if i < len(o.CRLPeers) {
			deps.CRLPeer, deps.RevocationKey = o.CRLPeers[i], ca.RevocationKey()
		}
		go func() {
			m, err := orchd.Start(context.Background(), cfg, deps)
			results <- result{m, err}
		}()
	}
	// A member of a new cluster returns once a quorum exists, so all start together.
	for range o.Members {
		r := <-results
		if r.err != nil {
			t.Fatalf("start member: %v", r.err)
		}
		c.Members = append(c.Members, r.o)
	}
	t.Cleanup(func() {
		for _, m := range c.Members {
			_ = m.Stop()
		}
	})
	return c
}

// Client returns a client of member i's API that presents id, or no certificate when id is nil.
func (c *Cluster) Client(t testing.TB, i int, id *Identity) *orchapi.Client {
	t.Helper()
	conf := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: c.CA.Pool()}
	if id != nil {
		conf.Certificates = []tls.Certificate{id.TLS}
	}
	cl, err := orchapi.New("https://"+c.Members[i].Addr(), orchapi.HTTPClient(conf, 30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return cl
}

// Leader waits until exactly one live member among those given leads and returns its index.
func (c *Cluster) Leader(t testing.TB, live []int, within time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var leaders []int
		for _, i := range live {
			if c.Members[i].IsLeader() {
				leaders = append(leaders, i)
			}
		}
		if len(leaders) == 1 {
			return leaders[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no single leader among members %v within %v", live, within)
	return -1
}

// SnapshotDir is where member i writes its snapshots.
func (c *Cluster) SnapshotDir(i int) string {
	return filepath.Join(c.dir, fmt.Sprintf("m%d", i), "snapshots")
}
