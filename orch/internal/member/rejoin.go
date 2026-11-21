package member

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/client/pkg/v3/transport"
	"go.etcd.io/etcd/client/pkg/v3/types"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/server/v3/storage/backend"
	"go.etcd.io/etcd/server/v3/storage/schema"
	"go.etcd.io/etcd/server/v3/storage/wal"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
	"go.uber.org/zap"

	"github.com/garamsh/naru/lib/atomicfile"
)

// askTimeout bounds one question to one peer.
const askTimeout = 10 * time.Second

// rejoinMarker is the file beside the data directory (DataDir + ".rejoin") that outlives the
// discarded directory: while it exists the member joins through its peers, whatever its flags say.
type rejoinMarker struct {
	// Peers are client URLs; the first is the one that answered.
	Peers []string `json:"peers"`
}

func markerPath(c Config) string { return filepath.Clean(c.DataDir) + ".rejoin" }

// RejoinPeers returns the client URLs of the peers c must join through, or none when the member
// continues from its data directory.
//
// A member with data asks the peers its own data lists (the membership its etcd backend holds,
// read with etcd's schema package), and c.Join when set, whether the cluster still lists a member of
// its name. When it does not, which is how a member the housekeeper removed finds out, the peers
// are written to a marker, the member directory is deleted and the peers are returned, the one that
// answered first. A marker with no member directory returns its peers again, so a crash between
// the discard and the join, or a join that failed, is retried at the next start. A cluster that
// cannot be asked leaves the directory.
//
// The member is matched by name, which a changed peer URL does not alter.
func RejoinPeers(ctx context.Context, c Config, log *slog.Logger) ([]string, error) {
	marked, err := readMarker(c)
	if err != nil {
		return nil, err
	}
	if !HasData(c.DataDir) {
		return marked, nil
	}
	peers, err := ownPeers(c)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, u := range slices.Concat(marked, peers, []string{c.Join}) {
		if u != "" && !slices.Contains(candidates, u) {
			candidates = append(candidates, u)
		}
	}
	members, answeredID, answered, err := ask(ctx, c, candidates)
	if err != nil || answered == "" {
		return nil, err
	}
	own, err := ownClusterID(c)
	if err != nil {
		return nil, fmt.Errorf("read the cluster ID of the member directory: %w", err)
	}
	if answeredID != own {
		log.Error("a peer answered from another cluster than the member's data belongs to; the member directory is kept",
			"member", c.Name, "peer", answered, "cluster", types.ID(answeredID).String(), "data", types.ID(own).String())
		return nil, nil
	}
	if slices.ContainsFunc(members, func(m *etcdserverpb.Member) bool { return m.Name == c.Name }) {
		return nil, removeMarker(c)
	}
	order := slices.Concat([]string{answered}, slices.DeleteFunc(slices.Clone(candidates), func(u string) bool { return u == answered }))
	data, err := json.Marshal(rejoinMarker{Peers: order})
	if err != nil {
		return nil, err
	}
	if err := atomicfile.Write(markerPath(c), data, 0o600); err != nil {
		return nil, fmt.Errorf("record the rejoin: %w", err)
	}
	if err := os.RemoveAll(filepath.Join(c.DataDir, "member")); err != nil {
		return nil, fmt.Errorf("discard the member the cluster removed: %w", err)
	}
	log.Warn("discarded the member directory of a member the cluster removed; it joins afresh", "member", c.Name, "via", answered)
	return order, nil
}

func readMarker(c Config) ([]string, error) {
	data, err := os.ReadFile(markerPath(c))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m rejoinMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", markerPath(c), err)
	}
	return m.Peers, nil
}

// removeMarker deletes the rejoin marker, when there is one: the member runs as a member of the
// cluster.
func removeMarker(c Config) error {
	if err := os.Remove(markerPath(c)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ownPeers lists the client URLs of the other members the member's own etcd backend holds. The
// backend panics on a corrupt database, as etcd does when it opens the same directory.
func ownPeers(c Config) ([]string, error) {
	db := filepath.Join(c.DataDir, "member", "snap", "db")
	if _, err := os.Stat(db); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	lg := zap.NewNop()
	be := backend.NewDefaultBackend(lg, db, backend.WithMmapSize(1<<20))
	defer func() { _ = be.Close() }()
	members, _ := schema.NewMembershipBackend(lg, be).MustReadMembersFromBackend()
	var urls []string
	for _, m := range members {
		if m.Name != c.Name && m.Name != "" && len(m.ClientURLs) > 0 && m.ClientURLs[0] != c.ClientURL {
			urls = append(urls, m.ClientURLs[0])
		}
	}
	slices.Sort(urls)
	return urls, nil
}

// ownClusterID is the ID of the cluster the member directory belongs to, from the metadata of its
// write-ahead log, read with etcd's wal package from the last snapshot the log holds, as etcd does
// at start.
func ownClusterID(c Config) (uint64, error) {
	dir := filepath.Join(c.DataDir, "member", "wal")
	lg := zap.NewNop()
	snaps, err := wal.ValidSnapshotEntries(lg, dir)
	if err != nil {
		return 0, err
	}
	var from walpb.Snapshot
	if len(snaps) > 0 {
		from = snaps[len(snaps)-1]
	}
	w, err := wal.OpenForRead(lg, dir, from)
	if err != nil {
		return 0, err
	}
	defer func() { _ = w.Close() }()
	raw, _, _, err := w.ReadAll()
	if err != nil {
		return 0, err
	}
	var meta etcdserverpb.Metadata
	if err := meta.Unmarshal(raw); err != nil {
		return 0, err
	}
	return meta.ClusterID, nil
}

// ask asks each of urls in turn for the member list and returns the first answer, the ID of the
// cluster that gave it and the URL that did; the URL is "" when none answered.
func ask(ctx context.Context, c Config, urls []string) ([]*etcdserverpb.Member, uint64, string, error) {
	if len(urls) == 0 {
		return nil, 0, "", nil
	}
	conf, err := clientTLS(c)
	if err != nil {
		return nil, 0, "", err
	}
	for _, u := range urls {
		if list, err := listMembers(ctx, conf, u); err == nil {
			return list.Members, list.Header.ClusterId, u, nil
		}
	}
	return nil, 0, "", nil
}

func listMembers(ctx context.Context, conf *tls.Config, url string) (*clientv3.MemberListResponse, error) {
	cl, err := clientv3.New(clientv3.Config{Endpoints: []string{url}, DialTimeout: askTimeout, TLS: conf})
	if err != nil {
		return nil, err
	}
	defer func() { _ = cl.Close() }()
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	return cl.MemberList(ctx)
}

// peerTLS is the TLS configuration etcd's peers and orch's reads of them use.
func peerTLS(c Config) transport.TLSInfo {
	return transport.TLSInfo{CertFile: c.Cert, KeyFile: c.Key, TrustedCAFile: c.CA, ClientCertAuth: true}
}
