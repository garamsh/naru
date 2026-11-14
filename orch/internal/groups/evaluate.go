package groups

import (
	"sort"
	"time"
)

// Observed is everything a pass knows: what the store holds and what the hosts report, read
// fresh at Now. Fences holds the fence of each stateful member whose host answered.
type Observed struct {
	Now     time.Time
	Groups  []GroupSpec
	Members []Member
	Hosts   []Host
	// Networks are the networks the groups may join.
	Networks []Network
	Fences   map[string]Fence
}

// MemberState is a member with what observation says of it.
type MemberState struct {
	Member
	// Phase is computed from observation; Member.Phase is the stored one.
	Phase      Phase
	HostReady  bool
	Machine    Machine
	HasMachine bool
	Outdated   bool
	Fence      Fence
}

// GroupState is a group with its members' states, in ID order.
type GroupState struct {
	Spec    GroupSpec
	Hash    string
	Invalid error
	Members []MemberState
}

// Desired is what the groups should be, judged against observation.
type Desired struct {
	Groups []GroupState
}

// Evaluate classifies every member of every group from observation alone.
func Evaluate(o Observed) Desired {
	hosts := make(map[string]Host, len(o.Hosts))
	for _, h := range o.Hosts {
		hosts[h.ID] = h
	}
	byGroup := map[string][]Member{}
	for _, m := range o.Members {
		byGroup[m.Group] = append(byGroup[m.Group], m)
	}
	specs := append([]GroupSpec(nil), o.Groups...)
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })

	var d Desired
	for _, spec := range specs {
		gs := GroupState{Spec: spec, Hash: spec.Template.Hash(), Invalid: spec.Validate()}
		members := byGroup[spec.Name]
		sort.Slice(members, func(i, j int) bool { return members[i].ordinal() < members[j].ordinal() })
		for _, m := range members {
			gs.Members = append(gs.Members, memberState(o, spec, gs.Hash, m, hosts))
		}
		d.Groups = append(d.Groups, gs)
	}
	return d
}

func memberState(o Observed, spec GroupSpec, hash string, m Member, hosts map[string]Host) MemberState {
	ms := MemberState{Member: m, Outdated: m.TemplateHash != hash}
	h, listed := hosts[m.Host]
	ms.HostReady = listed && h.Ready
	if listed {
		for _, mc := range h.Machines {
			if mc.ID == m.MachineID {
				ms.Machine, ms.HasMachine = mc, true
			}
		}
	}
	ms.Fence = o.Fences[m.ID]
	ms.Phase = phase(o.Now, spec, ms, h)
	return ms
}

func phase(now time.Time, spec GroupSpec, ms MemberState, h Host) Phase {
	switch {
	case ms.Member.Phase == PhaseGone:
		return PhaseGone
	case !ms.HostReady:
		if spec.Stateful && !fenceCleared(now, ms) {
			return PhaseFencedWaiting
		}
		return PhaseGone
	case !ms.HasMachine:
		return PhaseGone
	case spec.Stateful && !ms.Fence.LapsedAt.IsZero():
		return PhaseGone
	case h.Draining || len(constraintGaps(spec, h)) > 0:
		return PhaseDraining
	case ms.Machine.State == MachineRunning:
		return PhaseRunning
	}
	return PhasePending
}

// fenceCleared reports whether a stateful member on a host that is not ready can no longer
// be running: its host reports the fence lapsed, or the deadline orch last armed has passed.
func fenceCleared(now time.Time, ms MemberState) bool {
	if !ms.Fence.LapsedAt.IsZero() {
		return true
	}
	deadline := ms.FenceDeadline
	if ms.Fence.Deadline.After(deadline) {
		deadline = ms.Fence.Deadline
	}
	return now.After(deadline)
}
