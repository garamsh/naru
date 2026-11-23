package api

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// networkMembers is the members of a network, in three stages: the groups say which members exist,
// the hosts' microVMs say where each runs and at what address, and the hosts' attributes place it.
// A member whose group says it is gone is not listed; one no host reported is listed not running.
func networkMembers(network string, gs []orchapi.Group, ms []exposures.Machine, hs []orchapi.Host) []orchapi.NetworkMember {
	byName := map[string]exposures.Machine{}
	for _, m := range ms {
		byName[m.Name] = m
	}
	byHost := map[string]orchapi.Host{}
	for _, h := range hs {
		byHost[h.ID] = h
	}
	out := []orchapi.NetworkMember{}
	for _, g := range gs {
		if g.Spec.Template.Network != network {
			continue
		}
		for _, gm := range g.Members {
			if gm.Phase == orchapi.PhaseGone {
				continue
			}
			m := orchapi.NetworkMember{Name: gm.ID, Host: gm.Host, Phase: gm.Phase}
			if x, ok := byName[gm.ID]; ok {
				m.Host, m.Address, m.Reported, m.Running = x.Host, x.Address, true, x.Running
			}
			if h, ok := byHost[m.Host]; ok {
				m.Region, m.Zone = h.Region, h.Labels[orchapi.ZoneLabel]
			}
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// networkMembersList answers a reader for any network, and an exposer for its own: another
// network's members are not there for it, as another network's exposures are not.
func (h *handler) networkMembersList(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	name := r.PathValue("name")
	if who.Role == identity.RoleExposer {
		n, ok := identity.ExposerNetwork(who)
		if !ok {
			writeError(w, r, fmt.Errorf("%w: the exposer certificate names no network", exposures.ErrForbidden))
			return
		}
		if n != name {
			writeError(w, r, inventory.ErrNotFound)
			return
		}
	}
	if h.Machines == nil {
		writeError(w, r, errNoExposures)
		return
	}
	if _, err := h.Networks.Get(r.Context(), name); err != nil {
		writeError(w, r, err)
		return
	}
	gs, err := h.Groups.List(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	ms, err := h.Machines.Machines(r.Context(), name)
	if err != nil {
		writeError(w, r, err)
		return
	}
	hs, err := h.Inventory.Hosts(r.Context())
	reply(w, r, networkMembers(name, gs, ms, hs), err)
}
