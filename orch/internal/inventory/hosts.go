package inventory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

func (s *service) SetLabels(ctx context.Context, by, hostID string, req orchapi.SetLabelsRequest) (orchapi.Host, error) {
	if err := validAttributes(req.Region, req.Labels); err != nil {
		return orchapi.Host{}, err
	}
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventLabeled, Subject: hostID, Detail: map[string]string{"by": by, "region": req.Region}}
	h, err := s.store.UpdateHost(ctx, hostID, func(h *Host) { h.Region, h.Labels = req.Region, req.Labels }, &ev)
	if err != nil {
		return orchapi.Host{}, err
	}
	return h.api(), nil
}

func (s *service) Drain(ctx context.Context, by, hostID string) (orchapi.Host, error) {
	return s.setDraining(ctx, by, hostID, true, EventDrained)
}

func (s *service) Undrain(ctx context.Context, by, hostID string) (orchapi.Host, error) {
	return s.setDraining(ctx, by, hostID, false, EventUndrained)
}

func (s *service) setDraining(ctx context.Context, by, hostID string, draining bool, event string) (orchapi.Host, error) {
	ev := orchapi.Event{Time: s.clock.Now(), Type: event, Subject: hostID, Detail: map[string]string{"by": by}}
	h, err := s.store.UpdateHost(ctx, hostID, func(h *Host) { h.Draining = draining }, &ev)
	if err != nil {
		return orchapi.Host{}, err
	}
	return h.api(), nil
}

func (s *service) GrantProxy(ctx context.Context, by, hostID string) (orchapi.Host, error) {
	return s.setProxy(ctx, by, hostID, true, EventProxyGranted)
}

func (s *service) RevokeProxy(ctx context.Context, by, hostID string) (orchapi.Host, error) {
	return s.setProxy(ctx, by, hostID, false, EventProxyRevoked)
}

func (s *service) setProxy(ctx context.Context, by, hostID string, proxy bool, event string) (orchapi.Host, error) {
	ev := orchapi.Event{Time: s.clock.Now(), Type: event, Subject: hostID, Detail: map[string]string{"by": by}}
	h, err := s.store.UpdateHost(ctx, hostID, func(h *Host) { h.Proxy = proxy }, &ev)
	if err != nil {
		return orchapi.Host{}, err
	}
	return h.api(), nil
}

func (s *service) Heartbeat(ctx context.Context, hostID string) error {
	h, err := s.store.GetHost(ctx, hostID)
	if err != nil {
		return err
	}
	if a := h.Abandonment; a != nil {
		// An abandoned host that still reaches orch is the sign ADR 0109 names that its fence did
		// not hold. Its last-seen is left as it was, and the event is recorded once per member.
		if _, seen := s.abandonedBeats.LoadOrStore(hostID, true); !seen {
			state := "pending: " + a.Revocation.Error
			if a.Revocation.Revoked() {
				state = "revoked " + strings.Join(a.Revocation.Serials, ",")
			}
			ev := orchapi.Event{Time: s.clock.Now(), Type: EventAbandonedHeartbeat, Subject: hostID, Detail: map[string]string{"by": a.By, "fenced": a.Fenced, "revocation": state}}
			if err := s.store.AppendEvent(ctx, ev); err != nil {
				s.abandonedBeats.Delete(hostID)
				return err
			}
		}
		return fmt.Errorf("%w: %s was abandoned by %s at %s", ErrAbandoned, hostID, a.By, a.At.Format(time.RFC3339))
	}
	now := s.clock.Now()
	_, err = s.store.UpdateHost(ctx, hostID, func(h *Host) { h.LastSeen = now }, nil)
	return err
}

func (s *service) Hosts(ctx context.Context) ([]orchapi.Host, error) {
	hs, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.Host, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.api())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *service) Pending(ctx context.Context) ([]orchapi.Pending, error) {
	ps, err := s.store.ListPending(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.Pending, 0, len(ps))
	for _, p := range ps {
		out = append(out, orchapi.Pending{HostID: p.HostID, Code: identity.PairingCode(p.PublicKey), PublicKey: p.PublicKey, Address: p.Address, KeyID: p.KeyID, FirstSeen: p.FirstSeen, Hardware: p.Hardware})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.Before(out[j].FirstSeen) })
	return out, nil
}

func (s *service) Events(ctx context.Context, after string) ([]orchapi.Event, error) {
	return s.store.ListEvents(ctx, after)
}
