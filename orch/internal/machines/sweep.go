package machines

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// sweep reaches the microVMs on the host named as the reconciler names them that no current
// binding names. One of a removed Machine, which its tombstone names, is deleted with its disks;
// one of an older epoch of a live Machine is stopped and left with its disks, with an event, and
// one of an epoch the record lists abandoned is reported once per leader, its disks kept; one
// whose id has neither a record nor a tombstone, as after an etcd restore, is left as it is, with
// an event: a missing record is no deleted Machine (ADR 0109 item 7).
func (r *Reconciler) sweep(ctx context.Context, p *pass, host string) []error {
	var errs []error
	for _, vm := range p.hosts[host].MicroVMs {
		id, epoch, ok := parseMicroVM(vm.Name)
		if !ok {
			continue
		}
		m, known := p.machines[id]
		_, removed := p.tombstones[id]
		switch {
		case !known && removed:
			if err := r.cfg.Hosts.Delete(ctx, host, vm.Name); err != nil {
				errs = append(errs, fmt.Errorf("delete %s on %s: %w", vm.Name, host, err))
			}
		case !known:
			if r.note(host, vm.Name) {
				r.cfg.Log.Warn("a microVM is named as a Machine's that orch neither has nor removed; it is left as it is", "host", host, "microvm", vm.Name)
				r.fleetEvent(ctx, EventUnknownMicroVM, host+"/"+vm.Name, map[string]string{"host": host, "microvm": vm.Name, "id": id, "state": vm.State})
			}
		case m.binding != nil && m.binding.Host == host && m.binding.Epoch == epoch && !m.lost:
		default:
			// An older epoch, or the current one of a lost Machine, which no longer runs here.
			if vm.State == vmRunning || vm.State == vmPaused {
				if _, err := r.cfg.Hosts.Stop(ctx, host, vm.Name, 0); err != nil {
					errs = append(errs, fmt.Errorf("stop %s on %s: %w", vm.Name, host, err))
					continue
				}
				r.fleetEvent(ctx, EventOlderEpoch, m.caller+"/"+m.name, map[string]string{"host": host, "microvm": vm.Name, "id": id})
			}
			if a, ok := m.left(host, epoch); ok && r.note(host, "abandoned/"+vm.Name) {
				r.fleetEvent(ctx, EventAbandonedVolumesFound, m.caller+"/"+m.name, map[string]string{
					"host": host, "microvm": vm.Name, "id": id, "epoch": strconv.FormatUint(epoch, 10), "volumes": strings.Join(a.Volumes, ","),
				})
			}
		}
	}
	return errs
}

// microVMName is a name the reconciler gives: m-<id>-<epoch>.
var microVMName = regexp.MustCompile(`^` + MicroVMPrefix + `([0-9a-f]{16})-([1-9][0-9]*)$`)

func parseMicroVM(name string) (id string, epoch uint64, ok bool) {
	m := microVMName.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	epoch, err := strconv.ParseUint(m[2], 10, 64)
	return m[1], epoch, err == nil
}
