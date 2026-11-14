package groups

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ActionKind is what an Action does.
type ActionKind string

const (
	ActionCreate ActionKind = "create"
	ActionDelete ActionKind = "delete"
	ActionStop   ActionKind = "stop"
	ActionStart  ActionKind = "start"
	ActionRenew  ActionKind = "renew-fence"
	ActionForget ActionKind = "forget"
	// ActionHold records a decision to do nothing and changes nothing.
	ActionHold ActionKind = "hold"
)

// Action is one step of a plan, with the reason the planner took it.
type Action struct {
	Kind   ActionKind
	Group  string
	Member Member
	Host   string
	// Spec is what Create asks the host to make.
	Spec MachineSpec
	// Deadline is what Renew arms.
	Deadline time.Time
	Reason   string
}

func (a Action) String() string {
	switch a.Kind {
	case ActionCreate:
		return fmt.Sprintf("create %s on %s: %s", a.Member.ID, a.Host, a.Reason)
	case ActionRenew:
		return fmt.Sprintf("renew-fence %s on %s until %s: %s", a.Member.ID, a.Host, a.Deadline.Format(time.RFC3339), a.Reason)
	case ActionHold:
		return fmt.Sprintf("hold %s: %s", a.Group, a.Reason)
	}
	return fmt.Sprintf("%s %s on %s: %s", a.Kind, a.Member.ID, a.Host, a.Reason)
}

// Plan turns a desired state and the observation it was judged against into actions. It
// reads nothing else: the same inputs give the same plan.
func Plan(d Desired, o Observed) []Action {
	hosts := append([]Host(nil), o.Hosts...)
	var out []Action
	for _, gs := range d.Groups {
		out = append(out, planGroup(gs, o, hosts)...)
	}
	return out
}

func planGroup(gs GroupState, o Observed, hosts []Host) []Action {
	spec := gs.Spec
	hold := func(format string, a ...any) Action {
		return Action{Kind: ActionHold, Group: spec.Name, Reason: fmt.Sprintf(format, a...)}
	}
	if gs.Invalid != nil {
		return []Action{hold("%v", gs.Invalid)}
	}

	var out []Action
	var present, keep []MemberState
	var retire []retiring
	for _, ms := range gs.Members {
		switch ms.Phase {
		case PhaseGone:
			switch {
			case ms.HostReady && ms.HasMachine:
				out = append(out, Action{Kind: ActionDelete, Group: spec.Name, Member: ms.Member, Host: ms.Host, Reason: "member is gone but its machine is not"})
			case ms.Member.Phase != PhaseGone:
				out = append(out, Action{Kind: ActionForget, Group: spec.Name, Member: ms.Member, Host: ms.Host, Reason: "member is gone: its machine or its host is lost"})
			}
		case PhaseFencedWaiting:
			out = append(out, hold("%s on %s waits for its fence: deadline %s not passed and no lapse reported", ms.ID, ms.Host, ms.FenceDeadline.Format(time.RFC3339)))
			present = append(present, ms)
			keep = append(keep, ms)
		case PhaseDraining:
			present = append(present, ms)
			retire = append(retire, retiring{ms, "its host is draining or no longer meets the group's constraints"})
		default:
			present = append(present, ms)
			if ms.Outdated {
				retire = append(retire, retiring{ms, "its template is not the group's"})
			} else {
				keep = append(keep, ms)
			}
		}
	}

	for _, ms := range present {
		if spec.Stateful && ms.Phase != PhaseFencedWaiting && ms.Fence.Deadline.Sub(o.Now) < spec.FenceTTL/2 {
			out = append(out, Action{Kind: ActionRenew, Group: spec.Name, Member: ms.Member, Host: ms.Host, Deadline: o.Now.Add(spec.FenceTTL), Reason: "stateful member's fence is unarmed or half spent"})
		}
	}
	for _, ms := range keep {
		if ms.Phase == PhasePending && ms.Machine.State != MachineRunning {
			out = append(out, Action{Kind: ActionStart, Group: spec.Name, Member: ms.Member, Host: ms.Host, Reason: "machine is " + string(ms.Machine.State)})
		}
	}

	// Members beyond the count leave like members to retire.
	if surplus := len(keep) - spec.Count; surplus > 0 {
		var cand []MemberState
		for _, ms := range keep {
			if ms.Phase != PhaseFencedWaiting {
				cand = append(cand, ms)
			}
		}
		sort.SliceStable(cand, func(i, j int) bool {
			if ri, rj := cand[i].isRunning(), cand[j].isRunning(); ri != rj {
				return !ri
			}
			return cand[i].ordinal() > cand[j].ordinal()
		})
		for _, ms := range cand[:min(surplus, len(cand))] {
			retire = append(retire, retiring{ms, "the group has more members than its count"})
		}
	}

	out = append(out, planCreates(gs, o, hosts, present, keep, len(retire))...)

	available := 0
	for _, ms := range present {
		if ms.isRunning() {
			available++
		}
	}
	budget := max(0, available-spec.floor())
	sort.SliceStable(retire, func(i, j int) bool {
		if ri, rj := retire[i].isRunning(), retire[j].isRunning(); ri != rj {
			return !ri
		}
		return retire[i].ordinal() < retire[j].ordinal()
	})
	blocked := 0
	for _, r := range retire {
		act := Action{Kind: ActionDelete, Group: spec.Name, Member: r.Member, Host: r.Host, Reason: r.why}
		switch {
		case !r.isRunning():
		case budget == 0:
			blocked++
			continue
		default:
			budget--
			if spec.Stateful {
				act.Kind, act.Reason = ActionStop, r.why+"; stopped before it is deleted"
			}
		}
		out = append(out, act)
	}
	if blocked > 0 {
		out = append(out, hold("%d member(s) wait to leave: %d available, floor %d", blocked, available, spec.floor()))
	}
	return out
}

// retiring is a member that is to leave, and why.
type retiring struct {
	MemberState
	why string
}

func (ms MemberState) isRunning() bool {
	return ms.HostReady && ms.Machine.State == MachineRunning
}

// planCreates places as many new members as the count and the surge allow, replacing what
// is missing. A member waiting for its fence still holds its place in the count.
func planCreates(gs GroupState, o Observed, hosts []Host, present, keep []MemberState, retiring int) []Action {
	spec := gs.Spec
	deficit := spec.Count - len(keep)
	room := spec.Count + spec.Rollout.MaxSurge - len(present)
	n := min(deficit, room)
	if n <= 0 {
		return nil
	}
	onHost := map[string]int{}
	for _, ms := range present {
		onHost[ms.Host]++
	}
	next := 0
	for _, m := range o.Members {
		if m.Group == spec.Name {
			next = max(next, m.ordinal())
		}
	}
	var out []Action
	for range n {
		h, err := Place(spec, hosts, onHost)
		if err != nil {
			var pe *PlacementError
			if errors.As(err, &pe) {
				out = append(out, Action{Kind: ActionHold, Group: spec.Name, Reason: err.Error()})
			}
			break
		}
		next++
		id := fmt.Sprintf("%s-%d", spec.Name, next)
		m := Member{ID: id, Group: spec.Name, Host: h.ID, MachineID: id, TemplateHash: gs.Hash, Phase: PhasePending, LastSeen: o.Now}
		out = append(out, Action{
			Kind: ActionCreate, Group: spec.Name, Member: m, Host: h.ID,
			Spec:   MachineSpec{ID: id, Template: spec.Template, Fenced: spec.Stateful},
			Reason: createReason(len(keep), retiring, spec.Count),
		})
		onHost[h.ID]++
		for i := range hosts {
			if hosts[i].ID == h.ID {
				hosts[i].Capacity.CPUs -= spec.Template.CPUs
				hosts[i].Capacity.MemoryMiB -= spec.Template.MemoryMiB
			}
		}
	}
	return out
}

func createReason(keep, retire, count int) string {
	if retire > 0 {
		return fmt.Sprintf("replaces a member to retire: %d of %d current and placed", keep, count)
	}
	return fmt.Sprintf("%d of %d members present", keep, count)
}
