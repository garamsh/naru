package member

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// What bounds the space etcd takes (ADR 0088).
const (
	// CompactionRetention is how much history etcd keeps: it compacts every revision older than this,
	// every tenth of it.
	CompactionRetention = "10m"
	// maxFiles is how many WAL files and snapshot files etcd keeps.
	maxFiles = 2
	// QuotaShare is the share of the data disk the database may take: one eighth, so that a
	// defragmentation, which writes a second copy, and the WAL fit beside it.
	QuotaShare = 8
	// MinQuota and MaxQuota bound the quota derived from the disk. etcd advises at most 8 GiB; 2 GiB is
	// its own default and the most orch's inventory has any need of.
	MinQuota = 128 << 20
	MaxQuota = 2 << 30
	// MinDefragBytes is the database size under which a defragmentation is not worth its pause.
	MinDefragBytes = 32 << 20
	// DepartAfter is how long the leader sees a member unreachable before it removes the member, so
	// that it rejoins with an empty data directory (ADR 0088).
	DepartAfter = 30 * time.Minute
	// minMembers is the fewest members a cluster has for the leader to remove one.
	minMembers = 3
	// probeTimeout bounds one call to one member.
	probeTimeout = 10 * time.Second
	// defragTimeout bounds one defragmentation, which blocks its member.
	defragTimeout = 5 * time.Minute
)

// Quota is the backend quota for a data disk of diskBytes: a share of the disk, within MinQuota and
// MaxQuota.
func Quota(diskBytes uint64) int64 {
	return int64(min(max(diskBytes/QuotaShare, MinQuota), MaxQuota))
}

// DiskBytes is the size of the file system that holds path, or of the nearest directory above it
// that exists, since etcd makes its data directory itself.
func DiskBytes(path string) (uint64, error) {
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(path, &st)
		if err == nil {
			return st.Blocks * uint64(st.Bsize), nil
		}
		parent := filepath.Dir(path)
		if !os.IsNotExist(err) || parent == path {
			return 0, err
		}
		path = parent
	}
}

// ShouldDefrag reports whether a database of dbSize bytes of which inUse hold data is worth
// defragmenting: it is not small and at least half of it is free pages.
func ShouldDefrag(dbSize, inUse int64) bool {
	return dbSize >= MinDefragBytes && (dbSize-inUse)*2 >= dbSize
}

// Removable reports why a member may not be removed, or nil when it may: the cluster has at least
// three members and every member but the one removed answers, so the cluster keeps its quorum, and
// its fault tolerance, until the removed member is back.
func Removable(total int, othersReachable bool) error {
	if total < minMembers {
		return fmt.Errorf("the cluster has %d members; a member is removed only from %d or more", total, minMembers)
	}
	if !othersReachable {
		return fmt.Errorf("another member does not answer; removing one more would risk the quorum")
	}
	return nil
}

// Cluster is the part of etcd's client the housekeeper uses.
type Cluster interface {
	clientv3.Cluster
	clientv3.Maintenance
}

// Housekeeper keeps the members' databases small and replaces a member that stays out. Its Pass
// runs on the leader only, so one member at a time is defragmented and removed.
type Housekeeper struct {
	Client Cluster
	// Quota is the backend quota of the members, for the alarm to be disarmed once a member is below it.
	Quota int64
	// Departure is how long a member is unreachable before it is removed; DepartAfter when zero.
	Departure time.Duration
	Now       func() time.Time
	Log       *slog.Logger

	// since is when each unreachable member was first seen so, by ID. It starts empty on every
	// leader, which only delays a removal.
	since map[uint64]time.Time
}

type probe struct {
	member *etcdserverpb.Member
	status *clientv3.StatusResponse
}

// Pass looks at every member, defragments those whose database is mostly free pages one at a time
// while all of them answer, disarms the no-space alarm of a member that is below its quota, and
// removes the member that has stayed unreachable for the departure time when the cluster can spare
// it.
func (h *Housekeeper) Pass(ctx context.Context) error {
	list, err := h.Client.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	var up []probe
	var down []*etcdserverpb.Member
	for _, m := range list.Members {
		if len(m.ClientURLs) == 0 {
			down = append(down, m) // a member added but not yet started has no client URL
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		st, err := h.Client.Status(pctx, m.ClientURLs[0])
		cancel()
		if err != nil {
			down = append(down, m)
			continue
		}
		up = append(up, probe{m, st})
	}
	h.note(list.Members, down)

	if len(down) == 0 {
		for _, p := range up {
			if err := h.defrag(ctx, p); err != nil {
				return err
			}
		}
		return nil
	}
	return h.depart(ctx, list.Members, down)
}

// Forget drops what the housekeeper has seen of the members, for a member that no longer leads: a
// later term starts its own count of how long a member has been out.
func (h *Housekeeper) Forget() { h.since = nil }

// note remembers when each unreachable member first was, and forgets those that answer.
func (h *Housekeeper) note(all, down []*etcdserverpb.Member) {
	if h.since == nil {
		h.since = map[uint64]time.Time{}
	}
	now := h.Now()
	gone := map[uint64]bool{}
	for _, m := range down {
		gone[m.ID] = true
		if _, ok := h.since[m.ID]; !ok {
			h.since[m.ID] = now
		}
	}
	for _, m := range all {
		if !gone[m.ID] {
			delete(h.since, m.ID)
		}
	}
}

func (h *Housekeeper) defrag(ctx context.Context, p probe) error {
	if ShouldDefrag(p.status.DbSize, p.status.DbSizeInUse) {
		dctx, cancel := context.WithTimeout(ctx, defragTimeout)
		_, err := h.Client.Defragment(dctx, p.member.ClientURLs[0])
		cancel()
		if err != nil {
			return fmt.Errorf("defragment %s: %w", p.member.Name, err)
		}
		h.Log.Info("defragmented a member", "member", p.member.Name, "dbBytes", p.status.DbSize, "inUseBytes", p.status.DbSizeInUse)
	}
	return h.disarm(ctx, p)
}

// disarm clears the no-space alarm raised by a member whose database is now below the quota. etcd
// keeps the whole cluster read-only until someone does.
func (h *Housekeeper) disarm(ctx context.Context, p probe) error {
	st, err := h.Client.Status(ctx, p.member.ClientURLs[0])
	if err != nil || st.DbSize >= h.Quota {
		return nil
	}
	alarms, err := h.Client.AlarmList(ctx)
	if err != nil {
		return nil
	}
	for _, a := range alarms.Alarms {
		if a.Alarm != etcdserverpb.AlarmType_NOSPACE || a.MemberID != p.member.ID {
			continue
		}
		if _, err := h.Client.AlarmDisarm(ctx, (*clientv3.AlarmMember)(a)); err != nil {
			return fmt.Errorf("disarm the no-space alarm of %s: %w", p.member.Name, err)
		}
		h.Log.Info("disarmed the no-space alarm of a member", "member", p.member.Name, "dbBytes", st.DbSize)
	}
	return nil
}

// depart removes the first member that has been unreachable for the departure time, when it is
// the only one out and the cluster is large enough to spare it.
func (h *Housekeeper) depart(ctx context.Context, all, down []*etcdserverpb.Member) error {
	after := h.Departure
	if after == 0 {
		after = DepartAfter
	}
	now := h.Now()
	for _, m := range down {
		if now.Sub(h.since[m.ID]) < after {
			continue
		}
		if err := Removable(len(all), len(down) == 1); err != nil {
			h.Log.Warn("a member stays out and is not removed", "member", m.Name, "reason", err.Error())
			return nil
		}
		if _, err := h.Client.MemberRemove(ctx, m.ID); err != nil {
			return fmt.Errorf("remove %s: %w", m.Name, err)
		}
		delete(h.since, m.ID)
		h.Log.Warn("removed a member that stayed unreachable; it rejoins from its peers when it starts", "member", m.Name, "after", after.String())
		return nil
	}
	return nil
}
