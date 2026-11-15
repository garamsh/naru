package console

import (
	"fmt"
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
	// Hardware is what the host reported of itself, one line, or empty for a host that reported none.
	Hardware string
	Disks    []string
	Links    []string
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

	// The install-seed form: the lifetime and name it was submitted with, and where the seed points.
	NoSeed   bool
	SeedOrch string
	TTL      string
	SeedName string
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
		p.Pending = append(p.Pending, newPendingRow(h))
	}
	s.render(w, status, "pending", p)
}

func newPendingRow(h orchapi.Pending) pendingRow {
	row := pendingRow{Pending: h, Code: identity.PairingCode(h.PublicKey)}
	hw := h.Hardware
	if hw == nil {
		return row
	}
	row.Hardware = fmt.Sprintf("%d CPU, %d MiB memory", hw.CPUs, hw.MemoryBytes>>20)
	for _, d := range hw.Disks {
		row.Disks = append(row.Disks, fmt.Sprintf("%s %d GiB %s (%d partition(s))", d.Name, d.Size>>30, d.Model, d.Partitions))
	}
	for _, l := range hw.Links {
		carrier := "no carrier"
		if l.Carrier {
			carrier = "carrier"
		}
		row.Links = append(row.Links, fmt.Sprintf("%s %s (%s)", l.Name, l.MAC, carrier))
	}
	return row
}
