package groups

import (
	"context"
	"errors"
	"fmt"
)

// Apply carries out the actions in order and records an event for each, whether it worked or
// not. A failed action does not stop the rest: the next pass plans again from observation.
func (e *Engine) Apply(ctx context.Context, actions []Action) error {
	var errs []error
	for _, a := range actions {
		if err := e.apply(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			e.record(ctx, a, EventFailed, a.String()+": "+err.Error(), &errs)
			continue
		}
		e.record(ctx, a, eventKind(a.Kind), a.String(), &errs)
	}
	return errors.Join(errs...)
}

func eventKind(k ActionKind) EventKind {
	switch k {
	case ActionCreate:
		return EventCreate
	case ActionDelete:
		return EventDelete
	case ActionStop:
		return EventStop
	case ActionStart:
		return EventStart
	case ActionRenew:
		return EventRenew
	case ActionForget:
		return EventForget
	}
	return EventHold
}

func (e *Engine) record(ctx context.Context, a Action, kind EventKind, msg string, errs *[]error) {
	ev := Event{Time: e.clock.Now(), Kind: kind, Group: a.Group, Member: a.Member.ID, Host: a.Host, Message: msg}
	if err := e.events.Record(ctx, ev); err != nil {
		*errs = append(*errs, fmt.Errorf("record event: %w", err))
	}
}

func (e *Engine) apply(ctx context.Context, a Action) error {
	switch a.Kind {
	case ActionCreate:
		// The record comes first, so a crash after the host made the Machine leaves a
		// member the next pass finds; a crash before leaves a member it finds gone.
		stored, err := e.store.PutMember(ctx, a.Member)
		if err != nil {
			return err
		}
		if err := e.hosts.Create(ctx, a.Host, a.Spec); err != nil {
			stored.Phase = PhaseGone
			_, perr := e.store.PutMember(ctx, stored)
			return errors.Join(err, perr)
		}
	case ActionDelete:
		return e.hosts.Delete(ctx, a.Host, a.Member.MachineID)
	case ActionStop:
		return e.hosts.Stop(ctx, a.Host, a.Member.MachineID)
	case ActionStart:
		return e.hosts.Start(ctx, a.Host, a.Member.MachineID)
	case ActionRenew:
		if err := e.hosts.RenewFence(ctx, a.Host, a.Member.MachineID, a.Deadline); err != nil {
			return err
		}
		m := a.Member
		m.FenceDeadline, m.LastSeen = a.Deadline, e.clock.Now()
		_, err := e.store.PutMember(ctx, m)
		return err
	case ActionForget:
		m := a.Member
		m.Phase = PhaseGone
		_, err := e.store.PutMember(ctx, m)
		return err
	}
	return nil
}
