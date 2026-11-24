package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
)

// Exposures is what the host API asks of the host's exposed addresses: *network.Exposer.
type Exposures interface {
	Put(ctx context.Context, e network.Exposure, lease time.Duration) error
	Delete(ctx context.Context, a netip.Addr) error
	Exposures() []network.Exposure
}

const (
	maxExposures     = 1024
	maxExposurePorts = 64
	maxBackends      = 64
	maxLeaseSeconds  = 120
	// defaultLeaseSeconds is the lease of a request that names none (ADR 0085 item 5).
	defaultLeaseSeconds = 15
	proxyV2             = "v2"
)

func (s *Server) routeExposures(mux *http.ServeMux) {
	mux.HandleFunc("PUT /v1/exposures/{address}", s.putExposure)
	mux.HandleFunc("GET /v1/exposures", s.listExposures)
	mux.HandleFunc("DELETE /v1/exposures/{address}", s.deleteExposure)
}

// noExposures answers a call that needs exposures when the host was started without a LAN interface.
func (s *Server) noExposures(w http.ResponseWriter) bool {
	if s.deps.Exposures != nil {
		return false
	}
	writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: "this host was started without a LAN interface"})
	return true
}

func exposureView(e network.Exposure) hostapi.Exposure {
	v := hostapi.Exposure{Address: e.Address.String(), ExpiresAt: e.Deadline.UTC().Format(time.RFC3339)}
	// LeaseSeconds is what is left of the lease.
	v.LeaseSeconds = max(0, int((time.Until(e.Deadline)+time.Second-1)/time.Second))
	v.Network, v.Backends = e.Network, []string{}
	v.Ports = []hostapi.ExposurePort{}
	for _, p := range e.Ports {
		v.Ports = append(v.Ports, hostapi.ExposurePort{Protocol: p.Protocol, Port: p.Port, BackendPort: p.BackendPort})
	}
	for _, b := range e.Backends {
		v.Backends = append(v.Backends, b.String())
	}
	if e.Proxy {
		v.ProxyProtocol = proxyV2
	}
	v.ToProxy = e.ToProxy
	return v
}

// putExposure takes the lease on the address, or renews it. The lease ends leaseSeconds (15 when
// absent) after the host receives the request, unless a later one renews it.
func (s *Server) putExposure(w http.ResponseWriter, r *http.Request) {
	if s.noExposures(w) {
		return
	}
	var req hostapi.ExposureRequest
	if !decode(w, r, &req) {
		return
	}
	e, fields := parseExposure(r.PathValue("address"), req)
	if len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	if len(s.deps.Exposures.Exposures()) >= maxExposures && !s.holds(e.Address) {
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: "the host holds the most exposures it takes"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	lease := cmp.Or(req.LeaseSeconds, defaultLeaseSeconds)
	if err := s.deps.Exposures.Put(ctx, e, time.Duration(lease)*time.Second); err != nil {
		s.exposureFail(w, err)
		return
	}
	for _, x := range s.deps.Exposures.Exposures() {
		if x.Address == e.Address {
			writeJSON(w, http.StatusOK, exposureView(x))
			return
		}
	}
	writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: "the lease ran out as it was taken"})
}

func (s *Server) holds(a netip.Addr) bool {
	for _, x := range s.deps.Exposures.Exposures() {
		if x.Address == a {
			return true
		}
	}
	return false
}

// parseExposure reads the request, and returns the fields that are wrong.
func parseExposure(address string, req hostapi.ExposureRequest) (network.Exposure, []string) {
	var fields []string
	a, err := netip.ParseAddr(address)
	if err != nil || !a.Is4() || !a.IsGlobalUnicast() {
		fields = append(fields, "address")
	}
	if req.ToProxy {
		return parseToProxy(a, req, fields)
	}
	e := network.Exposure{Address: a, Network: req.Network}
	if !dnsLabel.MatchString(req.Network) {
		fields = append(fields, "network")
	}
	switch req.ProxyProtocol {
	case "":
	case proxyV2:
		e.Proxy = true
	default:
		fields = append(fields, "proxyProtocol")
	}
	if len(req.Ports) == 0 || len(req.Ports) > maxExposurePorts {
		fields = append(fields, "ports")
	}
	seen := map[network.ExposurePort]bool{}
	for i, p := range req.Ports {
		key := network.ExposurePort{Protocol: p.Protocol, Port: p.Port}
		bad := !p.Protocol.Valid() || p.Port < 1 || p.Port > 65535 || p.BackendPort < 1 || p.BackendPort > 65535 || seen[key]
		// A receiver that expects the PROXY header rejects a connection without it, and UDP has no
		// connection to write it on: the whole Service is TCP (ADR 0085 item 8).
		bad = bad || (e.Proxy && p.Protocol == wire.ProtocolUDP)
		seen[key] = true
		if bad {
			fields = append(fields, fmt.Sprintf("ports[%d]", i))
			continue
		}
		e.Ports = append(e.Ports, network.ExposurePort{Protocol: p.Protocol, Port: p.Port, BackendPort: p.BackendPort})
	}
	if len(req.Backends) == 0 || len(req.Backends) > maxBackends {
		fields = append(fields, "backends")
	}
	have := map[netip.Addr]bool{}
	for i, b := range req.Backends {
		x, err := netip.ParseAddr(b)
		if err != nil || !x.Is4() || !x.IsGlobalUnicast() || have[x] {
			fields = append(fields, fmt.Sprintf("backends[%d]", i))
			continue
		}
		have[x] = true
		e.Backends = append(e.Backends, x)
	}
	// A lease with no leaseSeconds is the default of ADR 0085 item 5.
	if req.LeaseSeconds < 0 || req.LeaseSeconds > maxLeaseSeconds {
		fields = append(fields, "leaseSeconds")
	}
	return e, fields
}

// parseToProxy reads a proxy exposure's lease (ADR 0112 item 8): no network, no backends and no
// PROXY protocol, and TCP 443, with 80 optional, each to the same port.
func parseToProxy(a netip.Addr, req hostapi.ExposureRequest, fields []string) (network.Exposure, []string) {
	e := network.Exposure{Address: a, ToProxy: true}
	if req.Network != "" {
		fields = append(fields, "network")
	}
	if len(req.Backends) > 0 {
		fields = append(fields, "backends")
	}
	if req.ProxyProtocol != "" {
		fields = append(fields, "proxyProtocol")
	}
	seen := map[int]bool{}
	for i, p := range req.Ports {
		if p.Protocol != wire.ProtocolTCP || p.Port != 443 && p.Port != 80 || p.BackendPort != p.Port || seen[p.Port] {
			fields = append(fields, fmt.Sprintf("ports[%d]", i))
			continue
		}
		seen[p.Port] = true
		e.Ports = append(e.Ports, network.ExposurePort{Protocol: p.Protocol, Port: p.Port, BackendPort: p.BackendPort})
	}
	if !seen[443] {
		fields = append(fields, "ports")
	}
	if req.LeaseSeconds < 0 || req.LeaseSeconds > maxLeaseSeconds {
		fields = append(fields, "leaseSeconds")
	}
	return e, fields
}

// exposureFail maps a refusal of the exposures to a status.
func (s *Server) exposureFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, network.ErrUnknownNetwork):
		invalid(w, "network")
	case errors.Is(err, network.ErrReserved):
		invalid(w, "address")
	case errors.Is(err, network.ErrBackend):
		invalid(w, "backends")
	case errors.Is(err, network.ErrHostAddress):
		invalid(w, "address")
	case errors.Is(err, network.ErrOutsideLAN):
		invalid(w, "address")
	case errors.Is(err, network.ErrTaken), errors.Is(err, network.ErrNoProxy):
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: err.Error()})
	case errors.Is(err, network.ErrInvalid):
		invalid(w, "exposure")
	case errors.Is(err, network.ErrNoLAN):
		writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: err.Error()})
	default:
		s.deps.Log.Error("cannot keep the exposures", "err", err.Error())
		writeJSON(w, http.StatusBadGateway, hostapi.ErrorBody{Error: err.Error()})
	}
}

func (s *Server) listExposures(w http.ResponseWriter, _ *http.Request) {
	if s.noExposures(w) {
		return
	}
	out := []hostapi.Exposure{}
	for _, e := range s.deps.Exposures.Exposures() {
		out = append(out, exposureView(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"exposures": out})
}

// deleteExposure releases the lease: the host stops announcing the address and forwarding it. An
// absent lease is 204.
func (s *Server) deleteExposure(w http.ResponseWriter, r *http.Request) {
	if s.noExposures(w) {
		return
	}
	a, err := netip.ParseAddr(r.PathValue("address"))
	if err != nil || !a.Is4() {
		invalid(w, "address")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.deps.Exposures.Delete(ctx, a); err != nil {
		s.exposureFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
