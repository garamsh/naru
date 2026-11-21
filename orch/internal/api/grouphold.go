package api

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// holdEvent is the type of the event the groups engine records when it places nothing, with the
// refused hosts and why in its message.
const holdEvent = "group.hold"

// withHold sets Hold on each group that has fewer live members than its count, from the newest
// hold event the log holds for that group. The log is read once, and only when a group is short.
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
	latest := map[string]orchapi.GroupHold{}
	for _, ev := range evs {
		if ev.Type == holdEvent && short[ev.Detail["group"]] {
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
