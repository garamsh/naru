package inventory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/orchapi"
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
	return p.Redacted().Marshal()
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

// allocate returns the lowest /24 of pool that is not in used. A pool of more than one /24 gives
// up its first, which holds the pool's own network address.
func allocate(pool netip.Prefix, used []netip.Prefix) (netip.Prefix, error) {
	taken := make(map[netip.Prefix]bool, len(used))
	for _, u := range used {
		taken[u] = true
	}
	base := pool.Addr().As4()
	start := binary.BigEndian.Uint32(base[:])
	count := uint32(1) << (layout.MachinePrefixBits - pool.Bits())
	for i := uint32(0); i < count; i++ {
		if i == 0 && count > 1 {
			continue
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], start+i<<(32-layout.MachinePrefixBits))
		p := netip.PrefixFrom(netip.AddrFrom4(b), layout.MachinePrefixBits)
		if !taken[p] {
			return p, nil
		}
	}
	return netip.Prefix{}, ErrPoolFull
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

// nextPrefix is the free prefix a new admission takes.
func (s *service) nextPrefix(ctx context.Context, pool netip.Prefix) (netip.Prefix, error) {
	as, err := s.store.ListAllocations(ctx)
	if err != nil {
		return netip.Prefix{}, err
	}
	used := make([]netip.Prefix, 0, len(as))
	for _, a := range as {
		if pf, err := netip.ParsePrefix(a.Prefix); err == nil {
			used = append(used, pf)
		}
	}
	return allocate(pool, used)
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
