package console

import (
	"net/http"
	"slices"
	"sort"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

type hostMicroVMs struct {
	Host     orchapi.Host
	MicroVMs []MicroVM
	Error    string
}

type pendingRow struct {
	orchapi.Pending
	Code string
}

// page is the data every page renders from; a page reads the fields it needs.
type page struct {
	Title   string
	Active  string
	Refresh bool
	Error   string
	Notice  string

	NoFleet  bool
	Hosts    []orchapi.Host
	MicroVMs []hostMicroVMs
	Groups   []Group
	Events   []orchapi.Event
	Pending  []pendingRow
	CSRF     string
	// Region and Labels refill the admit form after a refusal.
	Region string
	Labels string
}

// fail renders what went wrong reaching orch.
func (s *Server) fail(w http.ResponseWriter, name string, p page, err error) {
	p.Error = err.Error()
	s.cfg.Log.Warn("orch call", "page", name, "err", err)
	s.render(w, http.StatusBadGateway, name, p)
}

func (s *Server) hosts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.orchCtx(r)
	defer cancel()
	p := page{Title: "Hosts", Active: "hosts", Refresh: true}
	hosts, err := s.cfg.Orch.Hosts(ctx)
	if err != nil {
		s.fail(w, "hosts", p, err)
		return
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	p.Hosts = hosts
	s.render(w, http.StatusOK, "hosts", p)
}

func (s *Server) microvms(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.orchCtx(r)
	defer cancel()
	p := page{Title: "MicroVMs", Active: "microvms", Refresh: true}
	if s.cfg.Fleet == nil {
		p.NoFleet = true
		s.render(w, http.StatusOK, "microvms", p)
		return
	}
	hosts, err := s.cfg.Orch.Hosts(ctx)
	if err != nil {
		s.fail(w, "microvms", p, err)
		return
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	only := r.URL.Query().Get("host")
	for _, h := range hosts {
		if only != "" && h.ID != only {
			continue
		}
		row := hostMicroVMs{Host: h}
		vms, err := s.cfg.Fleet.MicroVMs(ctx, h.ID)
		if err != nil {
			row.Error = err.Error()
		}
		sort.Slice(vms, func(i, j int) bool { return vms[i].Name < vms[j].Name })
		row.MicroVMs = vms
		p.MicroVMs = append(p.MicroVMs, row)
	}
	s.render(w, http.StatusOK, "microvms", p)
}

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.orchCtx(r)
	defer cancel()
	p := page{Title: "Groups", Active: "groups", Refresh: true}
	if s.cfg.Fleet == nil {
		p.NoFleet = true
		s.render(w, http.StatusOK, "groups", p)
		return
	}
	groups, err := s.cfg.Fleet.Groups(ctx)
	if err != nil {
		s.fail(w, "groups", p, err)
		return
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	p.Groups = groups
	s.render(w, http.StatusOK, "groups", p)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.orchCtx(r)
	defer cancel()
	p := page{Title: "Events", Active: "events", Refresh: true}
	events, err := s.cfg.Orch.Events(ctx)
	if err != nil {
		s.fail(w, "events", p, err)
		return
	}
	// Newest first, and no more than a page holds.
	slices.Reverse(events)
	if len(events) > eventLimit {
		events = events[:eventLimit]
	}
	p.Events = events
	s.render(w, http.StatusOK, "events", p)
}

func (s *Server) pending(w http.ResponseWriter, r *http.Request) {
	p := page{Title: "Pending hosts", Active: "pending"}
	if id := r.URL.Query().Get("admitted"); id != "" {
		p.Notice = "Admitted " + id + "."
	}
	s.renderPending(w, r, http.StatusOK, p)
}

// renderPending lists the pending hosts, each with the pairing code of its key.
func (s *Server) renderPending(w http.ResponseWriter, r *http.Request, status int, p page) {
	ctx, cancel := s.orchCtx(r)
	defer cancel()
	list, err := s.cfg.Orch.Pending(ctx)
	if err != nil {
		s.fail(w, "pending", p, err)
		return
	}
	sort.Slice(list, func(i, j int) bool { return list[i].HostID < list[j].HostID })
	p.CSRF = s.csrf
	for _, h := range list {
		p.Pending = append(p.Pending, pendingRow{Pending: h, Code: identity.PairingCode(h.PublicKey)})
	}
	s.render(w, status, "pending", p)
}
