package inventory

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

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
		Guest:             p.Guest,
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
	if _, err := layout.ParseHostProfile(yaml); err != nil {
		if errors.Is(err, layout.ErrRedacted) {
			return fmt.Errorf("%w: %v", ErrRedacted, err)
		}
		return invalid("%v", err)
	}
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventProfileSet, Subject: "host-profile", Detail: map[string]string{"by": by}}
	return s.store.PutProfile(ctx, yaml, ev)
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
	return joinedPlan{profile: prof, addr: addr}, nil
}

// buildConfig is the naru.yaml of the joined host h.
func (j joinedPlan) buildConfig(h Host, prefix netip.Prefix, orchCA string) ([]byte, error) {
	n, err := j.profile.Node(layout.JoinedHost{
		ID: h.ID, Address: j.addr, Region: h.Region, Labels: h.Labels,
		MachinePrefix: prefix, Certificate: string(h.Certificate), OrchCA: orchCA,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return n.UserData()
}
