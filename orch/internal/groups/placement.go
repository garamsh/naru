package groups

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ErrNoHost is wrapped by the error Place returns when no host can take a member.
var ErrNoHost = errors.New("no host can take the member")

// PlacementError says why each host was refused.
type PlacementError struct {
	Group   string
	Refused map[string][]string
}

func (e *PlacementError) Error() string {
	ids := make([]string, 0, len(e.Refused))
	for id := range e.Refused {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+": "+strings.Join(e.Refused[id], ", "))
	}
	return fmt.Sprintf("%s: group %s: %s", ErrNoHost, e.Group, strings.Join(parts, "; "))
}

func (e *PlacementError) Unwrap() error { return ErrNoHost }

// constraintGaps lists the hard constraints of the group that h breaks: the ones a member
// already running on h also has to keep.
func constraintGaps(spec GroupSpec, h Host) []string {
	var gaps []string
	if h.Region != spec.Region {
		gaps = append(gaps, fmt.Sprintf("region %q is not %q", h.Region, spec.Region))
	}
	keys := make([]string, 0, len(spec.RequiredLabels))
	for k := range spec.RequiredLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := h.Labels[k]; !ok || v != spec.RequiredLabels[k] {
			gaps = append(gaps, fmt.Sprintf("required label %s=%s missing", k, spec.RequiredLabels[k]))
		}
	}
	return gaps
}

// placementGaps adds what only a new member needs of a host.
func placementGaps(spec GroupSpec, h Host) []string {
	gaps := constraintGaps(spec, h)
	if !h.Ready {
		gaps = append(gaps, "not ready")
	}
	if h.Draining {
		gaps = append(gaps, "draining")
	}
	if v := spec.Template.NodeVersion; v != "" && !slices.Contains(h.NodeVersions, v) {
		gaps = append(gaps, fmt.Sprintf("node version %s is outside the host's %v", v, h.NodeVersions))
	}
	if h.Capacity.CPUs < spec.Template.CPUs || h.Capacity.MemoryMiB < spec.Template.MemoryMiB {
		gaps = append(gaps, "not enough free capacity")
	}
	return gaps
}

func score(spec GroupSpec, h Host) int {
	s := 0
	for _, p := range spec.PreferredLabels {
		if v, ok := h.Labels[p.Key]; ok && v == p.Value {
			s += p.Weight
		}
	}
	return s
}

// Place picks the host for a new member of spec: among hosts that break no constraint, the
// highest preferred-label score, then the fewest members of the group (onHost counts them
// by host ID), then the most free memory, then the lowest ID. It reads nothing else.
func Place(spec GroupSpec, hosts []Host, onHost map[string]int) (Host, error) {
	var best *Host
	refused := map[string][]string{}
	for i := range hosts {
		h := hosts[i]
		if gaps := placementGaps(spec, h); len(gaps) > 0 {
			refused[h.ID] = gaps
			continue
		}
		if best == nil || better(spec, h, *best, onHost) {
			best = &hosts[i]
		}
	}
	if best == nil {
		return Host{}, &PlacementError{Group: spec.Name, Refused: refused}
	}
	return *best, nil
}

func better(spec GroupSpec, a, b Host, onHost map[string]int) bool {
	if sa, sb := score(spec, a), score(spec, b); sa != sb {
		return sa > sb
	}
	if na, nb := onHost[a.ID], onHost[b.ID]; na != nb {
		return na < nb
	}
	if a.Capacity.MemoryMiB != b.Capacity.MemoryMiB {
		return a.Capacity.MemoryMiB > b.Capacity.MemoryMiB
	}
	return a.ID < b.ID
}
