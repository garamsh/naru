package api

import (
	"context"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	groupsetcd "github.com/garamsh/naru/orch/internal/groups/etcd"
)

// holdEvent is the type of the event the groups engine records when it places nothing, with the
// refused hosts and why in its message.
const holdEvent = "group.hold"

// withHold sets Hold on each group that has fewer live members than its count, from the newest
// hold event the log holds for that group, when it is newer than twice the engine's repeat interval:
// the engine writes an ongoing hold at least once per interval, so an older one has ended, and twice
// covers one write missed after a change of leader. The log is read once, and only when a group is short.
func (h *handler) withHold(ctx context.Context, gs []orchapi.Group) error {
	short := map[string]bool{}
	for _, g := range gs {
		live := 0
		for _, m := range g.Members {
			if m.Phase != orchapi.PhaseGone {
				live++
			}
		}
		if live < g.Spec.Count {
			short[g.Spec.Name] = true
		}
	}
	if len(short) == 0 {
		return nil
	}
	evs, err := h.Inventory.Events(ctx, "")
	if err != nil {
		return err
	}
	now := time.Now()
	latest := map[string]orchapi.GroupHold{}
	for _, ev := range evs {
		if ev.Type == holdEvent && short[ev.Detail["group"]] && now.Sub(ev.Time) <= 2*groupsetcd.HoldRepeat {
			latest[ev.Detail["group"]] = orchapi.GroupHold{Time: ev.Time, Message: ev.Detail["message"]}
		}
	}
	for i := range gs {
		if hold, ok := latest[gs[i].Spec.Name]; ok {
			gs[i].Hold = &hold
		}
	}
	return nil
}
