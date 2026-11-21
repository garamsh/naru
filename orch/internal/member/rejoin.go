package member

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/etcdserver"
	"go.etcd.io/etcd/server/v3/etcdserver/api/rafthttp"
)

// DiscardIfRemoved deletes the member directory in c.DataDir when the cluster no longer lists a
// member named c.Name, which is how a member whose disk filled up rejoins: the leader removed it
// (Housekeeper), and a directory the cluster has forgotten can no longer be opened. It returns the
// client URL of the member that answered, for the caller to join through, or "" when the directory
// is kept. A cluster that cannot be asked, or one whose members publish no client URL, leaves the
// directory alone.
//
// A member started with --join asks that member. Any other asks the peers of its initial cluster,
// through the member list etcd serves its peers, so a member started with a static initial cluster
// rejoins as well. The member is matched by name, which a changed peer URL does not alter.
func DiscardIfRemoved(ctx context.Context, c Config, log *slog.Logger) (string, error) {
	members, via, err := clusterView(ctx, c)
	if err != nil || via == "" {
		return "", nil
	}
	if slices.ContainsFunc(members, func(m view) bool { return m.name == c.Name }) {
		return "", nil
	}
	if err := os.RemoveAll(filepath.Join(c.DataDir, "member")); err != nil {
		return "", fmt.Errorf("discard the member the cluster removed: %w", err)
	}
	log.Warn("discarded the member directory of a member the cluster removed; it joins afresh", "member", c.Name)
	return via, nil
}

// view is a member as the cluster lists it.
type view struct {
	name       string
	clientURLs []string
}

// clusterView lists the members of the cluster this member belongs to, and a client URL of one that
// answered and is not this member. It is an error, and an empty client URL, when none can be asked.
func clusterView(ctx context.Context, c Config) ([]view, string, error) {
	if c.Join != "" {
		list, err := askClients(ctx, c)
		return list, c.Join, err
	}
	var peers []string
	for _, part := range strings.Split(c.InitialCluster, ",") {
		name, url, ok := strings.Cut(part, "=")
		if ok && name != c.Name && url != c.PeerURL {
			peers = append(peers, url)
		}
	}
	if len(peers) == 0 {
		return nil, "", nil
	}
	rt, err := rafthttp.NewRoundTripper(peerTLS(c), 10*time.Second)
	if err != nil {
		return nil, "", err
	}
	cluster, err := etcdserver.GetClusterFromRemotePeers(nil, peers, rt)
	if err != nil {
		return nil, "", err
	}
	var list []view
	var via string
	for _, m := range cluster.Members() {
		list = append(list, view{m.Name, m.ClientURLs})
		if via == "" && m.Name != c.Name && len(m.ClientURLs) > 0 {
			via = m.ClientURLs[0]
		}
	}
	return list, via, nil
}

func askClients(ctx context.Context, c Config) ([]view, error) {
	conf, err := clientTLS(c)
	if err != nil {
		return nil, err
	}
	cl, err := clientv3.New(clientv3.Config{Endpoints: []string{c.Join}, DialTimeout: 10 * time.Second, TLS: conf})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cl.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := cl.MemberList(ctx)
	if err != nil {
		return nil, err
	}
	var list []view
	for _, m := range resp.Members {
		list = append(list, view{m.Name, m.ClientURLs})
	}
	return list, nil
}

// peerTLS is the TLS configuration etcd's peers and orch's reads of them use.
func peerTLS(c Config) transport.TLSInfo {
	return transport.TLSInfo{CertFile: c.Cert, KeyFile: c.Key, TrustedCAFile: c.CA, ClientCertAuth: true}
}
