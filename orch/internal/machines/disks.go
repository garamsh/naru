package machines

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/garamsh/naru/lib/machineapi"
)

// snapshot takes each pending snapshot of a Machine on the host: it records the host first, so a
// deletion that follows finds the disk snapshots, then takes one per volume, then marks it ready.
// A snapshot the host refuses MaxSnapshotRefusals times fails, and what it took is deleted.
func (r *Reconciler) snapshot(ctx context.Context, p *pass, host string, snaps []*SnapshotEntry) []error {
	var errs []error
	h := p.hosts[host]
	for _, s := range snaps {
		e := p.byID[s.Snapshot.Snapshot.MachineID]
		name := e.Binding.MicroVM(e.ID)
		if _, ok := h.microVM(name); !ok {
			continue
		}
		if s.Host == "" {
			if err := r.setSnapshot(ctx, s, nil, func(n *Snapshot) { n.Host = host }); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if s.Host != host {
			continue
		}
		var err error
		for i, v := range s.Snapshot.Snapshot.Spec.Volumes {
			if err = r.cfg.Hosts.SnapshotDisk(ctx, host, name, v.Name, s.DiskSnapshot(i)); err != nil {
				break
			}
		}
		switch {
		case err == nil:
			errs = append(errs, r.setSnapshot(ctx, s, nil, func(n *Snapshot) { n.Snapshot.Phase = machineapi.SnapshotReady }))
		case refused(err):
			he, _ := hostError(err)
			var del *Deletion
			if s.Refusals+1 >= MaxSnapshotRefusals {
				del = &Deletion{ID: s.ID, Host: host, Disks: s.DiskSnapshots()}
			}
			errs = append(errs, r.setSnapshot(ctx, s, del, func(n *Snapshot) {
				n.Refusals++
				if del != nil {
					n.Snapshot.Phase, n.Snapshot.Reason = machineapi.SnapshotFailed, "the host refused the snapshot: "+he.Message
				}
			}))
		default:
			errs = append(errs, fmt.Errorf("snapshot %s/%s: %w", s.Caller, s.Snapshot.Snapshot.Name, err))
		}
	}
	return errs
}

// disks deletes on the host the disk snapshots of each deletion not done, and then marks it done,
// so it stays as the snapshot's tombstone. A disk snapshot named as a snapshot's that a tombstone
// names is deleted too; one whose id has neither a snapshot nor a deletion is left, with an event.
func (r *Reconciler) disks(ctx context.Context, p *pass, host string) []error {
	var errs []error
	for _, d := range p.deletions {
		if d.Host != host || !d.Done.IsZero() {
			continue
		}
		var err error
		for _, disk := range d.Disks {
			if err = r.cfg.Hosts.DeleteDiskSnapshot(ctx, host, disk); err != nil {
				break
			}
		}
		if err == nil {
			d.Done = r.cfg.Clock.Now().UTC()
			err = r.cfg.Store.PutDeletion(ctx, d)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("delete the disk snapshots of %s on %s: %w", d.ID, host, err))
		}
	}
	for _, ds := range p.hosts[host].DiskSnapshots {
		id, ok := parseDiskSnapshot(ds.Name)
		if !ok || p.snapByID[id] != nil {
			continue
		}
		d, deleted := p.deletions[id]
		switch {
		case deleted && !d.Done.IsZero():
			if err := r.cfg.Hosts.DeleteDiskSnapshot(ctx, host, ds.Name); err != nil {
				errs = append(errs, fmt.Errorf("delete %s on %s: %w", ds.Name, host, err))
			}
		case deleted:
		case r.note(host, ds.Name):
			r.cfg.Log.Warn("a disk snapshot is named as a snapshot's that orch neither has nor deleted; it is left as it is", "host", host, "disk-snapshot", ds.Name)
			r.fleetEvent(ctx, EventUnknownDiskSnapshot, host+"/"+ds.Name, map[string]string{"host": host, "disk-snapshot": ds.Name, "id": id})
		}
	}
	return errs
}

// diskSnapshotName is a name the reconciler gives a disk snapshot: s-<snapshot id>-<volume>.
var diskSnapshotName = regexp.MustCompile(`^s-([0-9a-f]{16})-(0|[1-9][0-9]*)$`)

func parseDiskSnapshot(name string) (id string, ok bool) {
	m := diskSnapshotName.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	_, err := strconv.Atoi(m[2])
	return m[1], err == nil
}
