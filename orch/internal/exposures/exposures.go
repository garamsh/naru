// Package exposures is what orch does for the operator's exposures (ADR 0102): it keeps the
// address pools, assigns an address to each exposure out of a pool, and, as a reconciler, puts a
// lease on each address on one host.
package exposures

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// Errors the API maps to a status: a call outside the caller's network or pool is ErrForbidden
// (403), and a request that names something the fleet does not hold is ErrUnprocessable (422).
// ErrFull is the store's answer to a create over a network's limit.
var (
	ErrForbidden     = errors.New("the caller's network may not do this")
	ErrUnprocessable = errors.New("the request cannot be carried out")
	ErrFull          = errors.New("the network holds as many exposures as it may")
)

// Event types the service writes.
const (
	EventPoolApplied     = "address-pool.applied"
	EventPoolDeleted     = "address-pool.deleted"
	EventExposureApplied = "exposure.applied"
	EventExposureDeleted = "exposure.deleted"
)

// Limits bound what one network may hold (ADR 0085 item 7).
const (
	MaxExposuresPerNetwork = 64
	MaxPortsPerExposure    = 16
	MaxBackends            = 64
	maxPoolCIDRs           = 32
	maxPoolSize            = 1 << 16
)

// PhaseReleasing is the phase of an exposure its network deleted, or that is being removed: it
// keeps its address claimed until the host that held it confirmed the removal or its lease and
// Skew have passed, and then the reconciler deletes it.
const PhaseReleasing = "releasing"

// Record is an exposure as orch stores it: the exposure and the latest moment the lease given to
// its host may end.
type Record struct {
	orchapi.Exposure
	LeaseUntil     time.Time `json:"leaseUntil,omitzero"`
	LeasedBackends []string  `json:"leasedBackends,omitempty"`
}

// Equal reports whether two records store the same bytes.
func (r Record) Equal(o Record) bool {
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(o)
	return bytes.Equal(a, b)
}

// Claim is an address a pool has handed to an exposure.
type Claim struct {
	Address string
	Network string
	ID      string
}

// Store holds the pools and the exposures. An exposure is kept under its network and id, so an id
// is the network's own. Get and Delete of a pool and Get of an exposure return
// inventory.ErrNotFound for what is not there. Each write below holds only while what the caller
// read is still what is stored.
type Store interface {
	// PutPool writes p only while the stored pool is still prev (absent when prev is nil) and no
	// address of p has been claimed since check read the pool's claims; check refuses a pool the
	// claims do not fit.
	PutPool(ctx context.Context, p orchapi.AddressPool, prev *orchapi.AddressPool, check func(held []Claim) error) error
	GetPool(ctx context.Context, name string) (orchapi.AddressPool, error)
	// DeletePool returns inventory.ErrConflict while any address of the pool is claimed.
	DeletePool(ctx context.Context, name string) error
	ListPools(ctx context.Context) ([]orchapi.AddressPool, error)
	// CreateExposure writes r and claims its address in one transaction, while pool is still the
	// stored pool and the network holds fewer than limit exposures (ErrFull otherwise), and
	// returns inventory.ErrConflict when the id exists or the address is claimed.
	CreateExposure(ctx context.Context, r Record, pool orchapi.AddressPool, limit int) error
	// UpdateExposure writes r only while the stored record is still prev.
	UpdateExposure(ctx context.Context, r, prev Record) error
	GetExposure(ctx context.Context, network, id string) (Record, error)
	// DeleteExposure removes r and releases its address, only while r is still the stored record.
	DeleteExposure(ctx context.Context, r Record) error
	ListExposures(ctx context.Context) ([]Record, error)
}

// Networks reads the networks orch keeps.
type Networks interface {
	Get(ctx context.Context, name string) (orchapi.Network, error)
	List(ctx context.Context) ([]orchapi.Network, error)
}

// Members returns the addresses of the live microVMs on a network, as the hosts report them.
type Members interface {
	Addresses(ctx context.Context, network string) ([]string, error)
}

// Log is the inventory's event log.
type Log interface {
	AppendEvent(ctx context.Context, ev orchapi.Event) error
}

// Clock is the service's time.
type Clock interface {
	Now() time.Time
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	idRe   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,250}[a-z0-9])?$`)
)

// Service is the API's operations on pools and exposures.
type Service struct {
	store    Store
	networks Networks
	members  Members
	// profile is the host profile, whose mesh pool no address pool overlaps.
	profile inventory.ProfileReader
	log     Log
	clock   Clock
}

// New returns a Service.
func New(store Store, networks Networks, members Members, profile inventory.ProfileReader, log Log, clock Clock) *Service {
	return &Service{store: store, networks: networks, members: members, profile: profile, log: log, clock: clock}
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", inventory.ErrInvalid, fmt.Sprintf(format, a...))
}

func unprocessable(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnprocessable, fmt.Sprintf(format, a...))
}

func forbidden(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrForbidden, fmt.Sprintf(format, a...))
}

func parseCIDRs(cidrs []string) ([]netip.Prefix, error) {
	if len(cidrs) == 0 || len(cidrs) > maxPoolCIDRs {
		return nil, bad("a pool holds 1 to %d CIDRs", maxPoolCIDRs)
	}
	var out []netip.Prefix
	size := 0
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return nil, bad("cidr %q is not an IPv4 prefix with its host bits clear", c)
		}
		size += 1 << (32 - p.Bits())
		if size > maxPoolSize {
			return nil, bad("a pool holds at most %d addresses", maxPoolSize)
		}
		for _, o := range out {
			if o.Overlaps(p) {
				return nil, bad("cidrs %s and %s overlap", o, p)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func overlapsAny(a, b []netip.Prefix) (netip.Prefix, netip.Prefix, bool) {
	for _, x := range a {
		for _, y := range b {
			if x.Overlaps(y) {
				return x, y, true
			}
		}
	}
	return netip.Prefix{}, netip.Prefix{}, false
}

// ApplyPool creates the pool or replaces its spec. Every address is usable, the network and
// broadcast addresses of a CIDR included: the operator lists exactly the range the LAN's owner
// gave. A replacement that would leave an exposure's address outside the pool, or its network
// outside the pool's list, is refused; the store checks that against the claims it holds at the
// moment it writes, so an exposure created meanwhile cannot slip past.
func (s *Service) ApplyPool(ctx context.Context, by, name string, spec orchapi.AddressPoolSpec) (orchapi.AddressPool, error) {
	if !nameRe.MatchString(name) {
		return orchapi.AddressPool{}, bad("pool name %q is not a DNS label", name)
	}
	cidrs, err := parseCIDRs(spec.CIDRs)
	if err != nil {
		return orchapi.AddressPool{}, err
	}
	if len(spec.Networks) == 0 {
		return orchapi.AddressPool{}, bad("a pool lists the networks that may draw from it")
	}
	for _, n := range spec.Networks {
		if !nameRe.MatchString(n) {
			return orchapi.AddressPool{}, bad("network name %q is not a DNS label", n)
		}
	}
	nets := slices.Clone(spec.Networks)
	slices.Sort(nets)
	if len(slices.Compact(slices.Clone(nets))) != len(nets) {
		return orchapi.AddressPool{}, bad("a network is listed twice")
	}
	pools, err := s.store.ListPools(ctx)
	if err != nil {
		return orchapi.AddressPool{}, err
	}
	var prev *orchapi.AddressPool
	for _, o := range pools {
		if o.Name == name {
			prev = &o
		}
	}
	p := orchapi.AddressPool{Name: name, CIDRs: spec.CIDRs, Networks: nets}
	fits := func(held []Claim) error {
		if err := s.clear(ctx, name, cidrs); err != nil {
			return err
		}
		for _, c := range held {
			a, _ := netip.ParseAddr(c.Address)
			in := false
			for _, x := range cidrs {
				in = in || x.Contains(a)
			}
			if !in || !slices.Contains(nets, c.Network) {
				return fmt.Errorf("%w: exposure %s holds %s of network %s, which the new pool does not hold", inventory.ErrConflict, c.ID, c.Address, c.Network)
			}
		}
		return nil
	}
	if err := s.store.PutPool(ctx, p, prev, fits); err != nil {
		return orchapi.AddressPool{}, err
	}
	return p, s.log.AppendEvent(ctx, orchapi.Event{Time: s.clock.Now(), Type: EventPoolApplied, Subject: name, Detail: map[string]string{"by": by}})
}

// clear refuses a pool whose ranges overlap a network's prefix, another pool or the profile's mesh
// pool. The store runs it after it noted the address-space guard that a network write and a profile
// write also move, and writes the pool only while that guard is unchanged.
func (s *Service) clear(ctx context.Context, name string, cidrs []netip.Prefix) error {
	mesh, err := inventory.MeshPool(ctx, s.profile)
	if err != nil {
		return err
	}
	if mesh.IsValid() {
		if c, _, hit := overlapsAny(cidrs, []netip.Prefix{mesh}); hit {
			return bad("cidr %s overlaps the mesh pool %s", c, mesh)
		}
	}
	all, err := s.networks.List(ctx)
	if err != nil {
		return err
	}
	for _, n := range all {
		if p, err := netip.ParsePrefix(n.Prefix); err == nil {
			if c, _, hit := overlapsAny(cidrs, []netip.Prefix{p}); hit {
				return bad("cidr %s overlaps network %s (%s)", c, n.Name, n.Prefix)
			}
		}
	}
	pools, err := s.store.ListPools(ctx)
	if err != nil {
		return err
	}
	for _, o := range pools {
		if o.Name == name {
			continue
		}
		oc, _ := parseCIDRs(o.CIDRs)
		if a, b, hit := overlapsAny(cidrs, oc); hit {
			return bad("cidr %s overlaps %s of pool %s", a, b, o.Name)
		}
	}
	return nil
}

// Pools lists every pool.
func (s *Service) Pools(ctx context.Context) ([]orchapi.AddressPool, error) {
	return s.store.ListPools(ctx)
}

// DeletePool removes a pool no exposure draws from; the store refuses while an address is claimed.
func (s *Service) DeletePool(ctx context.Context, by, name string) error {
	if err := s.store.DeletePool(ctx, name); err != nil {
		return err
	}
	return s.log.AppendEvent(ctx, orchapi.Event{Time: s.clock.Now(), Type: EventPoolDeleted, Subject: name, Detail: map[string]string{"by": by}})
}

func validateSpec(spec orchapi.ExposureSpec) error {
	if n := len(spec.Ports); n == 0 || n > MaxPortsPerExposure {
		return bad("an exposure has 1 to %d ports", MaxPortsPerExposure)
	}
	seen := map[string]bool{}
	for _, p := range spec.Ports {
		if !p.Protocol.Valid() {
			return bad("protocol %q is not TCP or UDP", p.Protocol)
		}
		if p.Port < 1 || p.Port > 65535 {
			return bad("port %d must be 1 to 65535", p.Port)
		}
		if p.BackendPort < 1 || p.BackendPort > 65535 {
			return bad("backendPort %d must be 1 to 65535", p.BackendPort)
		}
		k := fmt.Sprintf("%s/%d", p.Protocol, p.Port)
		if seen[k] {
			return bad("port %s is listed twice", k)
		}
		seen[k] = true
	}
	if n := len(spec.Backends); n == 0 || n > MaxBackends {
		return bad("an exposure has 1 to %d backends", MaxBackends)
	}
	for _, b := range spec.Backends {
		if a, err := netip.ParseAddr(b); err != nil || !a.Is4() {
			return bad("backend %q is not an IPv4 address", b)
		}
	}
	switch spec.ProxyProtocol {
	case "":
	case orchapi.ProxyProtocolV2:
		for _, p := range spec.Ports {
			if p.Protocol != wire.ProtocolTCP {
				return unprocessable("port %d is %s, and the PROXY protocol is TCP only", p.Port, p.Protocol)
			}
		}
	default:
		return bad("proxyProtocol %q is not v2", spec.ProxyProtocol)
	}
	return nil
}

// own refuses an empty network: an exposure call acts on one network, never on every one. A name
// that is no DNS label names no network, so it never reaches a key.
func own(network string) error {
	if network == "" {
		return forbidden("the call names no network")
	}
	if !nameRe.MatchString(network) {
		return bad("network name %q is not a DNS label", network)
	}
	return nil
}

// ApplyExposure creates the operator's exposure of network or replaces its spec. A spec for
// another network and a pool that does not list the network are ErrForbidden. An id is the
// network's own, so another network's exposure of the same id is not seen. A new exposure gets the
// requested address when it is in the pool and free, else the lowest free address.
func (s *Service) ApplyExposure(ctx context.Context, by, network, id string, spec orchapi.ExposureSpec) (orchapi.Exposure, error) {
	if err := own(network); err != nil {
		return orchapi.Exposure{}, err
	}
	if !idRe.MatchString(id) {
		return orchapi.Exposure{}, bad("exposure id %q is not valid", id)
	}
	if spec.Network != network {
		return orchapi.Exposure{}, forbidden("the spec names network %q, not %q", spec.Network, network)
	}
	if err := validateSpec(spec); err != nil {
		return orchapi.Exposure{}, err
	}
	pool, err := s.store.GetPool(ctx, spec.Pool)
	if errors.Is(err, inventory.ErrNotFound) || err == nil && !slices.Contains(pool.Networks, network) {
		// A missing pool and a pool that does not list the network look alike to the caller.
		return orchapi.Exposure{}, forbidden("pool %q is not open to network %q", spec.Pool, network)
	}
	if err != nil {
		return orchapi.Exposure{}, err
	}
	if _, err := s.networks.Get(ctx, network); err != nil {
		return orchapi.Exposure{}, err
	}
	have, err := s.members.Addresses(ctx, network)
	if err != nil {
		return orchapi.Exposure{}, err
	}
	for _, b := range spec.Backends {
		if !slices.Contains(have, b) {
			return orchapi.Exposure{}, unprocessable("backend %s is not the address of a live member of network %s", b, network)
		}
	}
	cur, err := s.store.GetExposure(ctx, network, id)
	if err == nil {
		if cur.Owner != orchapi.OwnerOperator {
			return orchapi.Exposure{}, fmt.Errorf("%w: exposure %s is owned by the %s", inventory.ErrConflict, id, cur.Owner)
		}
		return s.update(ctx, by, cur, spec)
	}
	if !errors.Is(err, inventory.ErrNotFound) {
		return orchapi.Exposure{}, err
	}
	return s.create(ctx, by, id, spec, pool)
}

// update changes the ports, backends and proxy protocol of cur. The reconciler rewrites the record
// on every pass, so a write over a record that has just been renewed is read again and tried again.
func (s *Service) update(ctx context.Context, by string, cur Record, spec orchapi.ExposureSpec) (orchapi.Exposure, error) {
	var err error
	for try := range deleteTries {
		if try > 0 {
			if cur, err = s.store.GetExposure(ctx, cur.Network, cur.ID); err != nil {
				return orchapi.Exposure{}, err
			}
		}
		if cur.Phase == PhaseReleasing {
			return orchapi.Exposure{}, fmt.Errorf("%w: exposure %s is being released; apply it again once it is gone", inventory.ErrConflict, cur.ID)
		}
		if cur.Pool != spec.Pool || spec.RequestedAddress != "" && spec.RequestedAddress != cur.Address {
			return orchapi.Exposure{}, fmt.Errorf("%w: exposure %s keeps its pool and address; delete it to change them", inventory.ErrConflict, cur.ID)
		}
		next := cur
		next.Ports, next.Backends, next.ProxyProtocol = spec.Ports, slices.Clone(spec.Backends), spec.ProxyProtocol
		if next.Equal(cur) {
			return next.Exposure, nil
		}
		if err = s.store.UpdateExposure(ctx, next, cur); err == nil {
			return next.Exposure, s.event(ctx, EventExposureApplied, by, next)
		}
		if !errors.Is(err, inventory.ErrConflict) {
			return orchapi.Exposure{}, err
		}
	}
	return orchapi.Exposure{}, err
}

func (s *Service) create(ctx context.Context, by, id string, spec orchapi.ExposureSpec, pool orchapi.AddressPool) (orchapi.Exposure, error) {
	cidrs, err := parseCIDRs(pool.CIDRs)
	if err != nil {
		return orchapi.Exposure{}, err
	}
	recs, err := s.store.ListExposures(ctx)
	if err != nil {
		return orchapi.Exposure{}, err
	}
	taken := map[netip.Addr]bool{}
	for _, r := range recs {
		if a, err := netip.ParseAddr(r.Address); err == nil && r.Pool == pool.Name {
			taken[a] = true
		}
	}
	var addr netip.Addr
	if spec.RequestedAddress != "" {
		a, err := netip.ParseAddr(spec.RequestedAddress)
		in := false
		for _, c := range cidrs {
			in = in || err == nil && c.Contains(a)
		}
		if !in {
			return orchapi.Exposure{}, unprocessable("requested address %q is not in pool %s", spec.RequestedAddress, pool.Name)
		}
		if taken[a] {
			return orchapi.Exposure{}, fmt.Errorf("%w: address %s is held", inventory.ErrConflict, a)
		}
		addr = a
	} else {
		for _, c := range cidrs {
			for a := c.Addr(); c.Contains(a) && !addr.IsValid(); a = a.Next() {
				if !taken[a] {
					addr = a
				}
			}
		}
		if !addr.IsValid() {
			return orchapi.Exposure{}, fmt.Errorf("%w: pool %s has no free address", inventory.ErrConflict, pool.Name)
		}
	}
	r := Record{Exposure: orchapi.Exposure{
		ID: id, Network: spec.Network, Owner: orchapi.OwnerOperator, Pool: pool.Name, Address: addr.String(), Ports: spec.Ports,
		Backends: slices.Clone(spec.Backends), ProxyProtocol: spec.ProxyProtocol, Phase: orchapi.ExposurePending,
	}}
	// The claim in the same transaction is what keeps two requests from one address, the pool
	// check keeps a create from landing in a pool that was replaced or deleted meanwhile, and the
	// limit is counted there too: a lost race is a conflict, and the caller's next try plans from
	// what the store holds.
	err = s.store.CreateExposure(ctx, r, pool, MaxExposuresPerNetwork)
	if errors.Is(err, ErrFull) {
		return orchapi.Exposure{}, fmt.Errorf("%w: network %s holds %d exposures", inventory.ErrConflict, spec.Network, MaxExposuresPerNetwork)
	}
	if err != nil {
		return orchapi.Exposure{}, err
	}
	return r.Exposure, s.event(ctx, EventExposureApplied, by, r)
}

func (s *Service) event(ctx context.Context, typ, by string, r Record) error {
	return s.log.AppendEvent(ctx, orchapi.Event{Time: s.clock.Now(), Type: typ, Subject: r.ID, Detail: map[string]string{"by": by, "network": r.Network, "address": r.Address}})
}

// Exposure returns one exposure of network, of whichever owner. Another network's exposure and an
// absent one are inventory.ErrNotFound, and an exposure being released is not shown.
func (s *Service) Exposure(ctx context.Context, network, id string) (orchapi.Exposure, error) {
	if err := own(network); err != nil {
		return orchapi.Exposure{}, err
	}
	if !idRe.MatchString(id) {
		return orchapi.Exposure{}, bad("exposure id %q is not valid", id)
	}
	r, err := s.store.GetExposure(ctx, network, id)
	if err != nil {
		return orchapi.Exposure{}, err
	}
	if r.Phase == PhaseReleasing {
		return orchapi.Exposure{}, inventory.ErrNotFound
	}
	return r.Exposure, nil
}

// AllExposures lists every network's exposures, those being released included.
func (s *Service) AllExposures(ctx context.Context) ([]orchapi.Exposure, error) {
	recs, err := s.store.ListExposures(ctx)
	if err != nil {
		return nil, err
	}
	out := []orchapi.Exposure{}
	for _, r := range recs {
		out = append(out, r.Exposure)
	}
	return out, nil
}

// deleteTries bounds how often a delete or an update re-reads the record the reconciler renewed under it.
const deleteTries = 3

// DeleteExposure starts the release of an exposure of network, of whichever owner: the address stays claimed, and so
// cannot be given to another exposure, until the host that held it confirmed the removal or its
// lease and Skew have passed, and then the reconciler deletes the record. An absent exposure,
// another network's included, and one already being released are not an error.
func (s *Service) DeleteExposure(ctx context.Context, by, network, id string) error {
	if err := own(network); err != nil {
		return err
	}
	if !idRe.MatchString(id) {
		return bad("exposure id %q is not valid", id)
	}
	var err error
	for range deleteTries {
		var cur Record
		cur, err = s.store.GetExposure(ctx, network, id)
		if errors.Is(err, inventory.ErrNotFound) || err == nil && cur.Phase == PhaseReleasing {
			return nil
		}
		if err != nil {
			return err
		}
		next := cur
		next.Phase = PhaseReleasing
		if err = s.store.UpdateExposure(ctx, next, cur); err == nil {
			return s.event(ctx, EventExposureDeleted, by, cur)
		}
		if !errors.Is(err, inventory.ErrConflict) {
			return err
		}
	}
	return err
}
