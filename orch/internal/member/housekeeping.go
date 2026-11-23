package member

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	// MinQuota is the quota below which orch's inventory of a small fleet does not fit. MaxQuota bounds
	// the quota derived from the disk: etcd advises at most 8 GiB; 2 GiB is its own default and the
	// most orch's inventory has any need of.
	MinQuota = 128 << 20
	MaxQuota = 2 << 30
	// DefragHighWater is the percent of the quota a database must reach before it is defragmented, and
	// DefragReclaimable the percent of the file that must be free pages for the pause to be worth it (ADR 0103).
	DefragHighWater   = 80
	DefragReclaimable = 10
	// DisarmShare is the share of the quota a database must be under for its no-space alarm to be
	// cleared: half, so that the alarm does not return at the next write.
	DisarmShare = 2
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

// MinDisk is the smallest data disk a member runs on: the one at which the share is MinQuota. A
// smaller disk cannot hold a database, the copy a defragmentation writes and the WAL, so the member
// refuses to start on it.
const MinDisk = MinQuota * QuotaShare

// Quota is the backend quota for a data disk of diskBytes: a share of the disk, at most MaxQuota. A
// disk under MinDisk is refused.
func Quota(diskBytes uint64) (int64, error) {
	if diskBytes < MinDisk {
		return 0, fmt.Errorf("the data disk is %d bytes; etcd needs at least %d to keep a database, the copy a defragmentation writes and the WAL", diskBytes, uint64(MinDisk))
	}
	return int64(min(diskBytes/QuotaShare, MaxQuota)), nil
}

// BackendQuota is the quota the member runs with: QuotaBytes when it is set, else a share of the
// size of the disk that holds the data directory.
func (c Config) BackendQuota() (int64, error) {
	if c.QuotaBytes > 0 {
		return c.QuotaBytes, nil
	}
	disk, err := DiskBytes(c.DataDir)
	if err != nil {
		return 0, fmt.Errorf("size of the disk holding %s: %w", c.DataDir, err)
	}
	return Quota(disk)
}

// DiskBytes is the size of the file system that holds path (its total size, not what is free, which
// shrinks as etcd fills it), or of the nearest directory above it that exists, since etcd makes its
// data directory itself.
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
// defragmenting under quota: it is at least DefragHighWater percent of it and at least
// DefragReclaimable percent of the file is free pages.
func ShouldDefrag(quota, dbSize, inUse int64) bool {
	return dbSize*100 >= quota*DefragHighWater && (dbSize-inUse)*100 >= dbSize*DefragReclaimable
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
	// Fence, when set, is called just before a removal and fails it when the pass no longer leads
	// (ADR 0071), so a leader that paused past the end of its term removes nothing.
	Fence func(ctx context.Context) error
	// Report, when set, records that the live data of the named member is over a DisarmShare of the
	// quota, which no defragmentation can free: the quota is to be raised (ADR 0088). It is called
	// once per member for as long as the pass leads; a cluster whose no-space alarm is armed accepts
	// no write, so there the record fails and only the log says so.
	Report func(ctx context.Context, member string, detail map[string]string) error

	// since is when each unreachable member was first seen so, by ID, and unstartedSince when each
	// member that was added and has not started was. They start empty on every leader, which only
	// delays a removal.
	since, unstartedSince map[uint64]time.Time
	// reported is the members Report has been called for, by ID.
	reported map[uint64]bool
}

type probe struct {
	member *etcdserverpb.Member
	status *clientv3.StatusResponse
}

// Pass looks at every member, defragments those whose database is mostly free pages one at a time
// while all of them answer, disarms the no-space alarm of a member that is below its quota, and
// removes the member that has stayed unreachable for the departure time when the cluster can spare
// it, and the member that was added and has stayed unstarted for it.
func (h *Housekeeper) Pass(ctx context.Context) error {
	list, err := h.Client.MemberList(ctx)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	var up []probe
	var down, unstarted []*etcdserverpb.Member
	for _, m := range list.Members {
		if len(m.ClientURLs) == 0 {
			// Added and not yet started, so it has published no client URL. A learner takes no part in
			// the quorum, so only a voter is a member that must start or go.
			if !m.IsLearner {
				unstarted = append(unstarted, m)
			}
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
	h.note(list.Members, down, unstarted)

	var errs []error
	alarms, err := h.Client.AlarmList(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list the alarms: %w", err))
		alarms = &clientv3.AlarmResponse{}
	}
	// A defragmentation blocks its member, so it runs only while every member answers. Clearing an
	// alarm does not, and is the one thing a cluster with a member out needs from a member that
	// answers.
	for _, p := range up {
		armed := slices.IndexFunc(alarms.Alarms, func(a *etcdserverpb.AlarmMember) bool {
			return a.Alarm == etcdserverpb.AlarmType_NOSPACE && a.MemberID == p.member.ID
		})
		if len(down) == 0 {
			errs = append(errs, h.defrag(ctx, p, armed >= 0))
		}
		if armed >= 0 {
			errs = append(errs, h.disarm(ctx, p, alarms.Alarms[armed]))
		}
		h.checkFit(ctx, p)
	}
	if len(down) > 0 || len(unstarted) > 0 {
		errs = append(errs, h.depart(ctx, list.Members, down, unstarted))
	}
	return errors.Join(errs...)
}

// Forget drops what the housekeeper has seen of the members, for a member that no longer leads: a
// later term starts its own count of how long a member has been out, and reports again.
func (h *Housekeeper) Forget() { h.since, h.unstartedSince, h.reported = nil, nil, nil }

// note remembers when each unreachable member, and each member that was added and has not started,
// first was so, and forgets those that are neither.
func (h *Housekeeper) note(all, down, unstarted []*etcdserverpb.Member) {
	if h.since == nil {
		h.since = map[uint64]time.Time{}
	}
	if h.unstartedSince == nil {
		h.unstartedSince = map[uint64]time.Time{}
	}
	now := h.Now()
	gone, joining := map[uint64]bool{}, map[uint64]bool{}
	for _, m := range down {
		gone[m.ID] = true
		if _, ok := h.since[m.ID]; !ok {
			h.since[m.ID] = now
			h.Log.Info("a member does not answer", "member", m.Name)
		}
	}
	for _, m := range unstarted {
		joining[m.ID] = true
		if _, ok := h.unstartedSince[m.ID]; !ok {
			h.unstartedSince[m.ID] = now
			h.Log.Info("a member was added and has not started", "member", m.Name)
		}
	}
	for _, m := range all {
		if !gone[m.ID] {
			delete(h.since, m.ID)
		}
		if !joining[m.ID] {
			delete(h.unstartedSince, m.ID)
		}
	}
}

// defrag defragments a member whose database ShouldDefrag, or whose no-space alarm is armed and
// whose database is over the line for disarming it while its data is under: freeing the pages is
// what lets the alarm be cleared.
func (h *Housekeeper) defrag(ctx context.Context, p probe, armed bool) error {
	line := h.Quota / DisarmShare
	frees := armed && p.status.DbSize >= line && p.status.DbSizeInUse < line
	if !frees && !ShouldDefrag(h.Quota, p.status.DbSize, p.status.DbSizeInUse) {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, defragTimeout)
	defer cancel()
	if _, err := h.Client.Defragment(dctx, p.member.ClientURLs[0]); err != nil {
		return fmt.Errorf("defragment %s: %w", p.member.Name, err)
	}
	h.Log.Info("defragmented a member", "member", p.member.Name, "dbBytes", p.status.DbSize, "inUseBytes", p.status.DbSizeInUse)
	return nil
}

// disarm clears the no-space alarm raised by a member whose database, as it is now, is under a
// DisarmShare of the quota. etcd keeps the whole cluster read-only until someone does.
func (h *Housekeeper) disarm(ctx context.Context, p probe, alarm *etcdserverpb.AlarmMember) error {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	st, err := h.Client.Status(pctx, p.member.ClientURLs[0])
	cancel()
	if err != nil || st.DbSize >= h.Quota/DisarmShare {
		return nil
	}
	if _, err := h.Client.AlarmDisarm(ctx, (*clientv3.AlarmMember)(alarm)); err != nil {
		return fmt.Errorf("disarm the no-space alarm of %s: %w", p.member.Name, err)
	}
	h.Log.Info("disarmed the no-space alarm of a member", "member", p.member.Name, "dbBytes", st.DbSize)
	return nil
}

// checkFit logs at error level, and reports once, that the live data of a member is over the line
// the alarm is disarmed under. Compaction and defragmentation cannot free it, so once the database
// fills the quota the cluster stays read-only until the quota is raised.
func (h *Housekeeper) checkFit(ctx context.Context, p probe) {
	if p.status.DbSizeInUse < h.Quota/DisarmShare {
		return
	}
	h.Log.Error("the data in a member's etcd database is over half its quota, which compaction and defragmentation cannot free; raise etcdQuotaBytes in spec.orch and reboot the control hosts",
		"member", p.member.Name, "inUseBytes", p.status.DbSizeInUse, "quotaBytes", h.Quota)
	if h.Report == nil || h.reported[p.member.ID] {
		return
	}
	detail := map[string]string{"inUseBytes": strconv.FormatInt(p.status.DbSizeInUse, 10), "quotaBytes": strconv.FormatInt(h.Quota, 10)}
	if err := h.Report(ctx, p.member.Name, detail); err != nil {
		h.Log.Warn("the quota event was not recorded", "member", p.member.Name, "err", err.Error())
		return
	}
	if h.reported == nil {
		h.reported = map[uint64]bool{}
	}
	h.reported[p.member.ID] = true
}

// depart removes the first member that has stayed unstarted for the departure time, else the first
// that has been unreachable for it, when it is the only one out and the cluster is large enough to
// spare it. A member inside its window as unstarted is not out: it has not answered yet because it
// has not started, and counting it would keep a departed member from being removed.
func (h *Housekeeper) depart(ctx context.Context, all, down, unstarted []*etcdserverpb.Member) error {
	after := h.Departure
	if after == 0 {
		after = DepartAfter
	}
	now := h.Now()
	// An unstarted voter counts toward the quorum, so each one added to a cluster whose join then
	// failed costs it fault tolerance until it is gone. Removing it only lowers what the quorum
	// needs, so it takes no check of the others.
	for _, m := range unstarted {
		if now.Sub(h.unstartedSince[m.ID]) < after {
			continue
		}
		if err := h.remove(ctx, m); err != nil {
			return err
		}
		delete(h.unstartedSince, m.ID)
		h.Log.Warn("removed a member that was added and stayed unstarted; it joins again when it starts", "member", m.Name, "after", after.String())
		return nil
	}
	for _, m := range down {
		if now.Sub(h.since[m.ID]) < after {
			continue
		}
		if err := Removable(len(all), len(down) == 1); err != nil {
			h.Log.Warn("a member stays out and is not removed", "member", m.Name, "reason", err.Error())
			return nil
		}
		if err := h.remove(ctx, m); err != nil {
			return err
		}
		delete(h.since, m.ID)
		h.Log.Warn("removed a member that stayed unreachable; it rejoins from its peers when it starts", "member", m.Name, "after", after.String())
		return nil
	}
	return nil
}

// remove removes a member after the fence, which fails once the pass no longer leads. The fence
// writes nothing, so a database that the no-space alarm keeps read-only does not refuse it.
func (h *Housekeeper) remove(ctx context.Context, m *etcdserverpb.Member) error {
	if h.Fence != nil {
		if err := h.Fence(ctx); err != nil {
			return fmt.Errorf("remove %s: %w", m.Name, err)
		}
	}
	if _, err := h.Client.MemberRemove(ctx, m.ID); err != nil {
		return fmt.Errorf("remove %s: %w", m.Name, err)
	}
	return nil
}
