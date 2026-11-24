package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/prefixes"
)

// maxProfile bounds the profile a caller sends.
const maxProfile = 1 << 20

// Profile is the profile with every secret redacted: no role reads one back.
func (s *service) Profile(ctx context.Context) ([]byte, error) {
	b, err := s.store.GetProfile(ctx)
	if err != nil {
		return nil, err
	}
	p, err := layout.ParseHostProfile(b)
	if err != nil {
		return nil, err
	}
	return shown(p).Marshal()
}

// shown is the profile as GET /v1/profile shows it. It is built field by field from a list of what
// may be shown, so a field the profile gains later is not shown until it is listed here; a registry
// keeps its host, CA, user name and helper, and a password it holds is replaced by layout.Redacted.
func shown(p layout.HostProfile) layout.HostProfile {
	out := layout.HostProfile{
		FirecrackerSHA256: p.FirecrackerSHA256,
		TimeServers:       p.TimeServers,
		AWS:               p.AWS,
		MetricsListen:     p.MetricsListen,
		Network:           p.Network,
		Storage:           p.Storage,
		Extensions:        p.Extensions,
		Orch:              p.Orch,
		APIClientCA:       p.APIClientCA,
		MachinePool:       p.MachinePool,
		MeshPool:          p.MeshPool,
	}
	for _, r := range p.Registries {
		shownRegistry := layout.Registry{Host: r.Host, CA: r.CA, Username: r.Username, Helper: r.Helper}
		if r.Password != "" {
			shownRegistry.Password = layout.Redacted
		}
		out.Registries = append(out.Registries, shownRegistry)
	}
	return out
}

func (s *service) SetProfile(ctx context.Context, by string, yaml []byte) error {
	if len(yaml) > maxProfile {
		return invalid("the host profile is larger than %d bytes", maxProfile)
	}
	p, err := layout.ParseHostProfile(yaml)
	if err != nil {
		if errors.Is(err, layout.ErrRedacted) {
			return fmt.Errorf("%w: %v", ErrRedacted, err)
		}
		return invalid("%v", err)
	}
	old, err := MeshPool(ctx, s.store)
	if err != nil && !errors.Is(err, ErrConflict) {
		return err
	}
	var pool netip.Prefix
	if p.MeshPool != nil {
		pool = *p.MeshPool
	}
	var check func() error
	if pool != old {
		check = func() error { return s.meshPoolClear(ctx, pool) }
	}
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventProfileSet, Subject: "host-profile", Detail: map[string]string{"by": by}}
	return s.store.PutProfile(ctx, yaml, ev, check)
}

// meshPoolClear refuses a mesh pool (the zero prefix for none) that leaves outside it an address a
// host holds or is reserved, or that overlaps a network or an address pool. That it overlaps no
// Machine pool is the profile's own check.
func (s *service) meshPoolClear(ctx context.Context, pool netip.Prefix) error {
	held, err := s.store.ListAllocations(ctx, SpaceMesh)
	if err != nil {
		return err
	}
	for _, a := range held {
		if p, err := netip.ParsePrefix(a.Prefix); err != nil || !pool.IsValid() || !pool.Contains(p.Addr()) {
			return fmt.Errorf("%w: host %s holds the mesh address %s, which the profile's mesh pool must hold", ErrConflict, a.HostID, a.Prefix)
		}
	}
	if !pool.IsValid() {
		return nil
	}
	claims, err := s.space.Claimed(ctx)
	if err != nil {
		return err
	}
	for _, c := range claims {
		if c.Prefix.Overlaps(pool) {
			return fmt.Errorf("%w: the mesh pool %s overlaps %s of %s", ErrConflict, pool, c.Prefix, c.By)
		}
	}
	return nil
}

// meshPick is the pick of a host's mesh address. It reads the pool itself, so that the store, which
// compares the profile unchanged from before the pick to its write, never commits an address of a
// pool the profile no longer has.
func (s *service) meshPick(ctx context.Context) func([]netip.Prefix) (netip.Prefix, error) {
	return meshPickFrom(ctx, s.store)
}

func meshPickFrom(ctx context.Context, r ProfileReader) func([]netip.Prefix) (netip.Prefix, error) {
	return func(claimed []netip.Prefix) (netip.Prefix, error) {
		pool, err := MeshPool(ctx, r)
		if err != nil {
			return netip.Prefix{}, err
		}
		if !pool.IsValid() {
			return netip.Prefix{}, fmt.Errorf("%w: the profile has no mesh pool", ErrConflict)
		}
		return nextMeshAddress(pool, claimed)
	}
}

// ProfileReader reads the stored host profile, as Store does.
type ProfileReader interface {
	GetProfile(ctx context.Context) ([]byte, error)
}

// MeshPool returns the mesh pool of the stored profile, and the zero prefix when no profile is set
// or it has none. A stored profile that no longer checks is ErrConflict.
func MeshPool(ctx context.Context, r ProfileReader) (netip.Prefix, error) {
	raw, err := r.GetProfile(ctx)
	if errors.Is(err, ErrNotFound) {
		return netip.Prefix{}, nil
	}
	if err != nil {
		return netip.Prefix{}, err
	}
	p, err := layout.ParseHostProfile(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: the stored host profile: %v", ErrConflict, err)
	}
	if p.MeshPool == nil {
		return netip.Prefix{}, nil
	}
	return *p.MeshPool, nil
}

// nextMeshAddress picks a host's mesh address from the pool, as a /32, given the addresses claimed.
// A pool of more than two addresses gives up its first, the pool's own network address.
func nextMeshAddress(pool netip.Prefix, claimed []netip.Prefix) (netip.Prefix, error) {
	if pool.Bits() < 31 {
		claimed = append(claimed[:len(claimed):len(claimed)], netip.PrefixFrom(pool.Masked().Addr(), 32))
	}
	p, err := prefixes.Allocate(pool, 32, claimed)
	if errors.Is(err, prefixes.ErrFull) {
		return netip.Prefix{}, ErrMeshPoolFull
	}
	return p, err
}

func (s *service) Remove(ctx context.Context, by, hostID string) error {
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventRemoved, Subject: hostID, Detail: map[string]string{"by": by}}
	_, err := s.store.RemoveHost(ctx, hostID, ev)
	return err
}

// nextPrefix picks the Machine prefix of an admission from the pool, given the prefixes claimed.
// A pool of more than one /24 gives up its first, which holds the pool's own network address.
func nextPrefix(pool netip.Prefix, claimed []netip.Prefix) (netip.Prefix, error) {
	if pool.Bits() < layout.MachinePrefixBits {
		claimed = append(claimed[:len(claimed):len(claimed)], netip.PrefixFrom(pool.Masked().Addr(), layout.MachinePrefixBits))
	}
	p, err := prefixes.Allocate(pool, layout.MachinePrefixBits, claimed)
	if errors.Is(err, prefixes.ErrFull) {
		return netip.Prefix{}, ErrPoolFull
	}
	return p, err
}

// hostAddress is the IPv4 address of the host:port a joining host reported.
func hostAddress(s string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	return a.Unmap(), nil
}

// joinedPlan is what the admission of a host that joined from the install seed needs of orch before
// it asks the signer: the profile, and a free prefix.
type joinedPlan struct {
	profile layout.HostProfile
	addr    netip.Addr
	// crl is the signed revocation list the host's configuration starts with; nil when orch
	// has no revocation key.
	crl []byte
}

func (s *service) planJoined(ctx context.Context, p Pending) (joinedPlan, error) {
	raw, err := s.store.GetProfile(ctx)
	if errors.Is(err, ErrNotFound) {
		return joinedPlan{}, ErrNoProfile
	}
	if err != nil {
		return joinedPlan{}, err
	}
	prof, err := layout.ParseHostProfile(raw)
	if err != nil {
		return joinedPlan{}, fmt.Errorf("%w: the stored host profile: %v", ErrConflict, err)
	}
	addr, err := hostAddress(p.Address)
	if err != nil || !addr.Is4() {
		return joinedPlan{}, invalid("the address %q the host reported is not an IPv4 address", p.Address)
	}
	plan := joinedPlan{profile: prof, addr: addr}
	if s.conf.RevocationPublicKey != "" {
		if plan.crl, err = s.currentList(ctx); err != nil {
			return joinedPlan{}, err
		}
	}
	return plan, nil
}

// currentList is the signer's revocation list, checked against the key a joined host will verify it
// with, so that an admission does not hand out a configuration the host would refuse.
func (s *service) currentList(ctx context.Context) ([]byte, error) {
	key, err := identity.ParseRevocationPublic([]byte(s.conf.RevocationPublicKey))
	if err != nil {
		return nil, fmt.Errorf("%w: the revocation public key: %v", ErrConflict, err)
	}
	list, err := s.signer.CRL(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: the revocation list: %v", ErrSignerFailed, err)
	}
	if _, err := identity.VerifyCRL(key, list); err != nil {
		return nil, fmt.Errorf("%w: the revocation list: %v", ErrSignerFailed, err)
	}
	return list, nil
}

// buildConfig is the naru.yaml of the joined host h.
func (j joinedPlan) buildConfig(h Host, prefix netip.Prefix, orchCA, revocationKey string) ([]byte, error) {
	n, err := j.profile.Node(layout.JoinedHost{
		ID: h.ID, Address: j.addr, Region: h.Region, Labels: h.Labels,
		MachinePrefix: prefix, Certificate: string(h.Certificate), OrchCA: orchCA,
		RevocationPublicKey: revocationKey, CRL: string(j.crl),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return n.UserData()
}
