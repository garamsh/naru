// Package member runs one member of orch's embedded etcd and gives a client of it.
package member

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/embed"
)

// Cluster states a member starts in.
const (
	StateNew      = "new"
	StateExisting = "existing"
)

// readyTimeout bounds how long Start waits for the member to join a quorum.
const readyTimeout = 120 * time.Second

// Config is one member's place in the cluster.
type Config struct {
	Name           string
	DataDir        string
	PeerURL        string
	ClientURL      string
	InitialCluster string
	// State is StateNew or StateExisting.
	State string
	// Join, with StateExisting and a data directory that holds no member, is the client URL of a
	// running member. This member adds itself to that cluster and takes InitialCluster from what the
	// cluster answers.
	Join string
	// Cert, Key and CA are orch's own pair and the authority etcd trusts on both ports.
	Cert, Key, CA string
}

// Member is a running etcd member.
type Member struct {
	etcd   *embed.Etcd
	client *clientv3.Client
}

// Start runs the member and returns once it has joined a quorum.
func Start(ctx context.Context, c Config) (*Member, error) {
	if c.State != StateNew && c.State != StateExisting {
		return nil, fmt.Errorf("the cluster state %q is neither %q nor %q", c.State, StateNew, StateExisting)
	}
	peer, err := url.Parse(c.PeerURL)
	if err != nil || peer.Scheme != "https" {
		return nil, fmt.Errorf("peer URL %q is not an https URL", c.PeerURL)
	}
	client, err := url.Parse(c.ClientURL)
	if err != nil || client.Scheme != "https" {
		return nil, fmt.Errorf("client URL %q is not an https URL", c.ClientURL)
	}
	if c.Join != "" && c.State != StateExisting {
		return nil, fmt.Errorf("--join needs the cluster state %q", StateExisting)
	}
	if c.Join != "" && !HasData(c.DataDir) {
		initial, err := join(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("join %s: %w", c.Join, err)
		}
		c.InitialCluster = initial
	}
	tlsInfo := transport.TLSInfo{CertFile: c.Cert, KeyFile: c.Key, TrustedCAFile: c.CA, ClientCertAuth: true}

	cfg := embed.NewConfig()
	cfg.Name = c.Name
	cfg.Dir = c.DataDir
	cfg.ListenPeerUrls = []url.URL{*peer}
	cfg.AdvertisePeerUrls = []url.URL{*peer}
	cfg.ListenClientUrls = []url.URL{*client}
	cfg.AdvertiseClientUrls = []url.URL{*client}
	cfg.InitialCluster = c.InitialCluster
	cfg.ClusterState = c.State
	cfg.PeerTLSInfo = tlsInfo
	cfg.ClientTLSInfo = tlsInfo
	cfg.LogLevel = "error"

	e, err := embed.StartEtcd(cfg)
	if err != nil {
		return nil, fmt.Errorf("start etcd: %w", err)
	}
	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(readyTimeout):
		e.Close()
		return nil, errors.New("etcd did not join a quorum in time")
	case <-ctx.Done():
		e.Close()
		return nil, ctx.Err()
	}

	conf, err := clientTLS(c)
	if err != nil {
		e.Close()
		return nil, err
	}
	cl, err := clientv3.New(clientv3.Config{Endpoints: []string{c.ClientURL}, DialTimeout: 10 * time.Second, TLS: conf})
	if err != nil {
		e.Close()
		return nil, fmt.Errorf("etcd client: %w", err)
	}
	return &Member{etcd: e, client: cl}, nil
}

// HasData reports whether dir holds an etcd member, which a start continues and a restore or a
// join must not touch.
func HasData(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "member"))
	return err == nil
}

// join adds this member to the cluster the member at c.Join belongs to, unless an earlier attempt
// already did, and returns the initial cluster this member starts with: every member that has a
// name, and this one.
func join(ctx context.Context, c Config) (string, error) {
	conf, err := clientTLS(c)
	if err != nil {
		return "", err
	}
	cl, err := clientv3.New(clientv3.Config{Endpoints: []string{c.Join}, DialTimeout: 10 * time.Second, TLS: conf})
	if err != nil {
		return "", err
	}
	defer func() { _ = cl.Close() }()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	list, err := cl.MemberList(ctx)
	if err != nil {
		return "", err
	}
	members := list.Members
	if !slices.ContainsFunc(members, func(m *etcdserverpb.Member) bool { return slices.Contains(m.PeerURLs, c.PeerURL) }) {
		added, err := addWhenHealthy(ctx, cl, c.PeerURL)
		if err != nil {
			return "", err
		}
		members = added.Members
	}
	var parts []string
	for _, m := range members {
		if m.Name != "" && m.Name != c.Name && len(m.PeerURLs) > 0 {
			parts = append(parts, m.Name+"="+m.PeerURLs[0])
		}
	}
	return strings.Join(append(parts, c.Name+"="+c.PeerURL), ","), nil
}

// addWhenHealthy adds a member, and tries again while etcd refuses because the members it already
// has are not yet connected to each other for long enough, as a member that has just joined is not.
func addWhenHealthy(ctx context.Context, cl *clientv3.Client, peerURL string) (*clientv3.MemberAddResponse, error) {
	for {
		added, err := cl.MemberAdd(ctx, []string{peerURL})
		if err == nil {
			return added, nil
		}
		if e := rpctypes.Error(err); e != rpctypes.ErrUnhealthy && e != rpctypes.ErrMemberNotEnoughStarted {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w (last refusal: %v)", ctx.Err(), err)
		case <-time.After(time.Second):
		}
	}
}

// clientTLS is the TLS configuration of orch's own client of its member.
func clientTLS(c Config) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(c.Cert, c.Key)
	if err != nil {
		return nil, err
	}
	pem, err := os.ReadFile(c.CA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no certificate", c.CA)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, RootCAs: pool}, nil
}

// Client returns orch's client of this member.
func (m *Member) Client() *clientv3.Client { return m.client }

// Close stops the member. It resigns nothing: a leader's lease lapses on its own.
func (m *Member) Close() {
	_ = m.client.Close()
	m.etcd.Close()
}
