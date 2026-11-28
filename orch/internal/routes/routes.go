// Package routes keeps every proxy host's routing table (ADR 0112 items 5 and 6): the leader derives
// the table from the Machines and aliases at one revision of orch's store, puts it whole to every
// host that reports a proxy, and keeps the revision each one's proxy has loaded, which a Machine's
// delete waits on before its address can be given to another.
package routes

import (
	"cmp"
	"slices"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// Dropped is a route the table leaves out, because a proxy would drop it, and why.
type Dropped struct {
	Hostname string
	Reason   string
}

// Table is the routes of recs and aliases in the zone, sorted by hostname: each service of a placed
// Machine that is not deleting and whose host reported its address, under its stable name, and
// each alias of such a Machine. A route that fails Route.Check, and every route of a name given
// twice, is dropped and returned, as a proxy drops them, so one bad name never stops a host taking
// the rest.
func Table(zone string, recs []machines.Record, aliases []machines.Alias) ([]hostapi.Route, []Dropped) {
	routed := map[string]machines.Record{}
	var out []hostapi.Route
	for _, r := range recs {
		if r.Binding == nil || !r.PrivateAddress.IsValid() || r.Status.Phase == machineapi.PhaseDeleting {
			continue
		}
		routed[r.ID] = r
		for i, svc := range r.Spec.Services {
			name := r.ID + "." + zone
			if i > 0 {
				name = svc.Name + "--" + name
			}
			out = append(out, route(name, r, svc))
		}
	}
	for _, a := range aliases {
		r, ok := routed[a.MachineID]
		if !ok {
			continue
		}
		i := 0
		if a.Service != "" {
			i = slices.IndexFunc(r.Spec.Services, func(s machineapi.Service) bool { return s.Name == a.Service })
		}
		if i < 0 || i >= len(r.Spec.Services) {
			continue
		}
		rt := route(a.Hostname, r, r.Spec.Services[i])
		rt.Certificate, rt.Key = a.Certificate, a.Key
		out = append(out, rt)
	}
	slices.SortFunc(out, func(a, b hostapi.Route) int { return cmp.Compare(a.Hostname, b.Hostname) })
	names := map[string]int{}
	for _, rt := range out {
		names[rt.Hostname]++
	}
	var dropped []Dropped
	kept := out[:0]
	for _, rt := range out {
		if names[rt.Hostname] > 1 {
			dropped = append(dropped, Dropped{rt.Hostname, "the name is given twice"})
			continue
		}
		if _, err := rt.Check(); err != nil {
			dropped = append(dropped, Dropped{rt.Hostname, err.Error()})
			continue
		}
		kept = append(kept, rt)
	}
	return kept, dropped
}

func route(hostname string, r machines.Record, svc machineapi.Service) hostapi.Route {
	p := hostapi.RouteHTTP
	if svc.Protocol == machineapi.ServiceTCP {
		p = hostapi.RouteTCP
	}
	return hostapi.Route{
		Hostname: hostname, Machine: r.ID, Address: r.PrivateAddress, Port: int(svc.Port), Protocol: p,
		ProxyProtocol: svc.ProxyProtocol, Running: r.Status.Phase == machineapi.PhaseRunning,
	}
}
