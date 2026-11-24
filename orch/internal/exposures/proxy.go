package exposures

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// MaxProxyExposures bounds the proxy exposures, which no network's limit counts (ADR 0112 item 8).
const MaxProxyExposures = 64

// The ports a proxy exposure may carry: 443 for the proxy's TLS listeners, and 80 for its redirect
// to https.
const (
	ProxyPortTLS      = 443
	ProxyPortRedirect = 80
)

// A proxy exposure is stored as a Record of no network, whose ports go to the holding host's own
// naru-proxy at the same numbers and which has no backends. It shares the pools' claims, and so
// their address space, with the networks' exposures.

func proxyView(r Record) orchapi.ProxyExposure {
	ports := make([]int, 0, len(r.Ports))
	for _, p := range r.Ports {
		ports = append(ports, p.Port)
	}
	return orchapi.ProxyExposure{ID: r.ID, Pool: r.Pool, Address: r.Address, Ports: ports, Host: r.Host, Phase: r.Phase}
}

// proxyPorts checks a proxy exposure's ports, 443 and optionally 80, each once, and returns them as
// TCP ports in the order given.
func proxyPorts(ports []int) ([]orchapi.ExposurePort, error) {
	out := []orchapi.ExposurePort{}
	for _, p := range ports {
		if p != ProxyPortTLS && p != ProxyPortRedirect {
			return nil, bad("a proxy exposure's ports are %d and %d, not %d", ProxyPortTLS, ProxyPortRedirect, p)
		}
		if slices.ContainsFunc(out, func(e orchapi.ExposurePort) bool { return e.Port == p }) {
			return nil, bad("port %d is listed twice", p)
		}
		out = append(out, orchapi.ExposurePort{Protocol: wire.ProtocolTCP, Port: p, BackendPort: p})
	}
	if !slices.Contains(ports, ProxyPortTLS) {
		return nil, bad("a proxy exposure carries port %d", ProxyPortTLS)
	}
	return out, nil
}

// ApplyProxyExposure creates the proxy exposure or replaces its ports. It may draw from any pool
// (ADR 0112 item 8, amending ADR 0085 item 3), and a missing one is ErrForbidden, as a pool not
// open to a network is for a network's exposure. A new proxy exposure gets the requested address
// when it is in the pool and free, else the lowest free address; an existing one keeps its pool
// and address.
func (s *Service) ApplyProxyExposure(ctx context.Context, by, id string, spec orchapi.ProxyExposureSpec) (orchapi.ProxyExposure, error) {
	if !idRe.MatchString(id) {
		return orchapi.ProxyExposure{}, bad("proxy exposure id %q is not valid", id)
	}
	ports, err := proxyPorts(spec.Ports)
	if err != nil {
		return orchapi.ProxyExposure{}, err
	}
	pool, err := s.store.GetPool(ctx, spec.Pool)
	if errors.Is(err, inventory.ErrNotFound) {
		return orchapi.ProxyExposure{}, forbidden("there is no pool %q", spec.Pool)
	}
	if err != nil {
		return orchapi.ProxyExposure{}, err
	}
	cur, err := s.store.GetExposure(ctx, "", id)
	if err == nil {
		return s.updateProxy(ctx, by, cur, spec, ports)
	}
	if !errors.Is(err, inventory.ErrNotFound) {
		return orchapi.ProxyExposure{}, err
	}
	r := Record{Exposure: orchapi.Exposure{
		ID: id, Owner: orchapi.OwnerOperator, Pool: pool.Name, Ports: ports, Backends: []string{}, Phase: orchapi.ExposurePending,
	}}
	r, err = s.create(ctx, by, r, spec.RequestedAddress, pool, MaxProxyExposures)
	return proxyView(r), err
}

// updateProxy changes the ports of cur, retried as update is.
func (s *Service) updateProxy(ctx context.Context, by string, cur Record, spec orchapi.ProxyExposureSpec, ports []orchapi.ExposurePort) (orchapi.ProxyExposure, error) {
	var err error
	for try := range deleteTries {
		if try > 0 {
			if cur, err = s.store.GetExposure(ctx, "", cur.ID); err != nil {
				return orchapi.ProxyExposure{}, err
			}
		}
		if cur.Phase == PhaseReleasing {
			return orchapi.ProxyExposure{}, fmt.Errorf("%w: proxy exposure %s is being released; apply it again once it is gone", inventory.ErrConflict, cur.ID)
		}
		if cur.Pool != spec.Pool || spec.RequestedAddress != "" && spec.RequestedAddress != cur.Address {
			return orchapi.ProxyExposure{}, fmt.Errorf("%w: proxy exposure %s keeps its pool and address; delete it to change them", inventory.ErrConflict, cur.ID)
		}
		next := cur
		next.Ports = ports
		if next.Equal(cur) {
			return proxyView(next), nil
		}
		if err = s.store.UpdateExposure(ctx, next, cur); err == nil {
			return proxyView(next), s.event(ctx, EventProxyExposureApplied, by, next)
		}
		if !errors.Is(err, inventory.ErrConflict) {
			return orchapi.ProxyExposure{}, err
		}
	}
	return orchapi.ProxyExposure{}, err
}

// ProxyExposure returns one proxy exposure; one being released is not shown.
func (s *Service) ProxyExposure(ctx context.Context, id string) (orchapi.ProxyExposure, error) {
	if !idRe.MatchString(id) {
		return orchapi.ProxyExposure{}, bad("proxy exposure id %q is not valid", id)
	}
	r, err := s.store.GetExposure(ctx, "", id)
	if err != nil {
		return orchapi.ProxyExposure{}, err
	}
	if r.Phase == PhaseReleasing {
		return orchapi.ProxyExposure{}, inventory.ErrNotFound
	}
	return proxyView(r), nil
}

// ProxyExposures lists the proxy exposures, those being released included.
func (s *Service) ProxyExposures(ctx context.Context) ([]orchapi.ProxyExposure, error) {
	recs, err := s.store.ListExposures(ctx)
	if err != nil {
		return nil, err
	}
	out := []orchapi.ProxyExposure{}
	for _, r := range recs {
		if r.Network == "" {
			out = append(out, proxyView(r))
		}
	}
	return out, nil
}

// DeleteProxyExposure starts the release of a proxy exposure, as DeleteExposure does of a
// network's. An absent one and one already being released are not an error.
func (s *Service) DeleteProxyExposure(ctx context.Context, by, id string) error {
	if !idRe.MatchString(id) {
		return bad("proxy exposure id %q is not valid", id)
	}
	return s.release(ctx, by, "", id)
}
