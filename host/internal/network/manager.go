package network

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Errors a Manager answers; the API maps them to statuses.
var (
	ErrUnknownNetwork = errors.New("no such network")
	ErrOverlap        = errors.New("overlaps another network's prefix")
)

// Runner runs a program with stdin and returns its stdout.
type Runner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// ExecRunner runs programs on the host.
type ExecRunner struct{}

// Run runs name with args, feeding it stdin. A failure carries the program's stderr.
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Manager holds the host's networks, their members and the tunnel's peers, and applies them.
// Orch declares networks and peers through the host API; the members are the microVMs created on
// them. Every change applies the whole state, then writes it to State, so the host converges on
// what Manager holds and Load returns it to that after a restart.
type Manager struct {
	Run Runner
	// Tunnel is the host's end of the tunnel; nil leaves the host with no tunnel.
	Tunnel Tunnel
	Base   Config // Management, TapPrefix, Egress, LAN and Proxy, which do not change
	// State is the file the state is kept in; empty keeps it in memory only.
	State string
	// Now is the clock the leases' deadlines are read against; nil is time.Now.
	Now func() time.Time
	// Boot returns the ID of the current boot and the time since it began, which a wall-clock step
	// does not change; nil reads /proc. State keeps the leases against it.
	Boot func() (string, time.Duration, error)
	// Log reports a lease Load dropped; nil is silent.
	Log *slog.Logger

	mu  sync.Mutex
	c   Config
	ran bool // the host has had a state applied, so c is what the ruleset and the tunnel hold
}

// state is the file: everything a restart must bring back.
type state struct {
	Networks []Network `json:"networks"`
	Members  []Member  `json:"members"`
	Peers    []Peer    `json:"peers"`
	// Mesh is the host's mesh address, the zero value when orch declared none.
	Mesh netip.Addr `json:"meshAddress,omitzero"`
	// MeshPool is the prefix of the hosts' mesh addresses, the zero value when orch declared none.
	MeshPool netip.Prefix `json:"meshPool,omitzero"`
	// Boot and Exposures are the leases with the time they end, counted from the start of the boot
	// Boot names: a restart in the same boot restores a lease that has not run out, and a reboot,
	// which took the addresses and the rules with it, restores none.
	Boot      string       `json:"boot,omitempty"`
	Exposures []leaseState `json:"exposures,omitempty"`
}

// leaseState is a lease in the file: Until is when it ends, as a time since the start of the boot.
type leaseState struct {
	Exposure
	Until time.Duration `json:"until"`
}

// Load reads State, when it exists, and applies it, so the host is closed by the ruleset before
// the API serves, whatever State holds.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var next Config
	if m.State != "" {
		raw, err := os.ReadFile(m.State)
		switch {
		case err == nil:
			var s state
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("%s: %w", m.State, err)
			}
			next.Networks, next.Members, next.Peers, next.Mesh, next.MeshPool = s.Networks, s.Members, s.Peers, s.Mesh, s.MeshPool
			next.Exposures = m.restoreLeases(s, next)
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
	}
	return m.run(ctx, next)
}

// restoreLeases returns the leases of s that the host may still serve: those of this boot that have
// not run out and that are valid against the networks Load restored. It reads the time left from
// the boot's clock, so a wall clock that stepped while hostd was down changes nothing.
func (m *Manager) restoreLeases(s state, c Config) []Exposure {
	boot, up, err := m.boot()
	var out []Exposure
	for _, l := range s.Exposures {
		e := l.Exposure
		left := l.Until - up
		var why error
		switch {
		case err != nil:
			why = err
		case s.Boot != boot:
			why = errors.New("it is from another boot")
		case left <= 0:
			why = errors.New("it ran out")
		default:
			e.Deadline = m.now().Add(left)
			why = checkExposure(m.Base, c, e)
		}
		if why != nil {
			if m.Log != nil {
				// A lease that fails Validate is one a rule of the host no longer accepts, such as a file
				// written before a field was renamed: an operator has to act on it, so it is a warning.
				level := slog.LevelInfo
				if errors.Is(why, ErrInvalid) {
					level = slog.LevelWarn
				}
				m.Log.Log(context.Background(), level, "dropped a lease at load", "address", e.Address.String(), "reason", why.Error())
			}
			continue
		}
		out = append(out, e)
	}
	return out
}

func (m *Manager) boot() (string, time.Duration, error) {
	if m.Boot != nil {
		return m.Boot()
	}
	id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", 0, err
	}
	var up float64
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "", 0, err
	}
	if _, err := fmt.Sscan(string(raw), &up); err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(string(id)), time.Duration(up * float64(time.Second)), nil
}

// TunnelInfo returns the tunnel's public key, port and mesh address; false when the host has no
// tunnel.
func (m *Manager) TunnelInfo() (TunnelInfo, bool) {
	if m.Tunnel == nil {
		return TunnelInfo{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return TunnelInfo{PublicKey: m.Tunnel.PublicKey(), Port: m.Tunnel.Port(), Mesh: m.c.Mesh}, true
}

// Networks returns the declared networks, ordered by name.
func (m *Manager) Networks() []Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedNetworks(m.c.Networks)
}

// Network returns the network named name.
func (m *Manager) Network(name string) (Network, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.c.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return Network{}, false
}

// Members returns the attached microVMs, ordered by address.
func (m *Manager) Members() []Member {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Member(nil), m.c.Members...)
	sort.Slice(out, func(i, j int) bool { return out[i].Address.Less(out[j].Address) })
	return out
}

// Declare adds the network or replaces the one of its name, keeping the members attached to it, and
// applies. A prefix that overlaps another network's is refused.
func (m *Manager) Declare(ctx context.Context, n Network) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Networks = nil
	for _, o := range m.c.Networks {
		if o.Name == n.Name {
			continue
		}
		if o.Prefix.Overlaps(n.Prefix) {
			return fmt.Errorf("%w: %s", ErrOverlap, o.Name)
		}
		next.Networks = append(next.Networks, o)
	}
	next.Networks = append(next.Networks, n)
	return m.apply(ctx, next)
}

// Remove drops the network and its members, and applies. An absent network is not an error.
func (m *Manager) Remove(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Networks, next.Members, next.Exposures = nil, nil, nil
	for _, o := range m.c.Networks {
		if o.Name != name {
			next.Networks = append(next.Networks, o)
		}
	}
	for _, x := range m.c.Exposures {
		if x.Network != name {
			next.Exposures = append(next.Exposures, x)
		}
	}
	for _, x := range m.c.Members {
		if x.Network != name {
			next.Members = append(next.Members, x)
		}
	}
	return m.apply(ctx, next)
}

// Attach adds a member and applies. The member's address is whatever its host gave it. A microVM is
// attached once: a second Attach of the same address replaces the first.
func (m *Manager) Attach(ctx context.Context, x Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n *Network
	for i := range m.c.Networks {
		if m.c.Networks[i].Name == x.Network {
			n = &m.c.Networks[i]
		}
	}
	if n == nil {
		return ErrUnknownNetwork
	}
	next := m.c
	next.Members = nil
	for _, o := range m.c.Members {
		if o.Address != x.Address {
			next.Members = append(next.Members, o)
		}
	}
	next.Members = append(next.Members, x)
	next.Machines = slices.DeleteFunc(slices.Clone(m.c.Machines), func(a netip.Addr) bool { return a == x.Address })
	return m.apply(ctx, next)
}

// Detach drops the member or the Machine of address a, and applies. An absent one is not an error.
func (m *Manager) Detach(ctx context.Context, a netip.Addr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Members = nil
	for _, o := range m.c.Members {
		if o.Address != a {
			next.Members = append(next.Members, o)
		}
	}
	next.Machines = slices.DeleteFunc(slices.Clone(m.c.Machines), func(x netip.Addr) bool { return x == a })
	return m.apply(ctx, next)
}

// Reconcile drops the members and the Machines whose address no machine of the host holds, and
// applies. live lists the addresses of the host's machines; it is called with the Manager locked, so
// an Attach or an Admit is either before it and seen, or after and kept. A failure of live changes
// nothing.
func (m *Manager) Reconcile(ctx context.Context, live func(context.Context) ([]netip.Addr, error)) error {
	return m.reconcile(ctx, live, false)
}

// Adopt is Reconcile that also takes every address no member holds as a Machine's, for the pass at
// start, before the API serves, when no microVM is between its create and its Attach.
func (m *Manager) Adopt(ctx context.Context, live func(context.Context) ([]netip.Addr, error)) error {
	return m.reconcile(ctx, live, true)
}

func (m *Manager) reconcile(ctx context.Context, live func(context.Context) ([]netip.Addr, error), adopt bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	addrs, err := live(ctx)
	if err != nil {
		return fmt.Errorf("list the host's machines: %w", err)
	}
	held := make(map[netip.Addr]bool, len(addrs))
	for _, a := range addrs {
		held[a] = true
	}
	next := m.c
	next.Members, next.Machines = nil, nil
	member := map[netip.Addr]bool{}
	for _, x := range m.c.Members {
		if held[x.Address] {
			next.Members = append(next.Members, x)
			member[x.Address] = true
		}
	}
	machine := map[netip.Addr]bool{}
	for _, a := range m.c.Machines {
		machine[a] = true
	}
	for _, a := range addrs {
		if !member[a] && (machine[a] || adopt) {
			next.Machines = append(next.Machines, a)
			machine[a] = false
		}
	}
	return m.apply(ctx, next)
}

// Admit takes the addresses of a microVM put on no network as a Machine's, and applies when that
// changes anything. An address a member holds changes no rule, so apply keeps none of it.
func (m *Manager) Admit(ctx context.Context, addrs ...netip.Addr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Machines = append([]netip.Addr(nil), m.c.Machines...)
	for _, a := range addrs {
		if !slices.Contains(next.Machines, a) {
			next.Machines = append(next.Machines, a)
		}
	}
	if len(next.Machines) == len(m.c.Machines) {
		return nil
	}
	return m.apply(ctx, next)
}

// SetPeers replaces the tunnel's peers and the host's mesh address, and applies.
func (m *Manager) SetPeers(ctx context.Context, mesh netip.Addr, pool netip.Prefix, peers []Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Peers, next.Mesh, next.MeshPool = append([]Peer(nil), peers...), mesh, pool
	return m.apply(ctx, next)
}

// MeshPool returns the mesh pool orch declared, the zero value when it declared none.
func (m *Manager) MeshPool() netip.Prefix {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.MeshPool
}

// Mesh returns the host's mesh address, the zero value when it has none.
func (m *Manager) Mesh() netip.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.c.Mesh
}

// Peers returns the tunnel's peers, ordered by public key.
func (m *Manager) Peers() []Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedPeers(m.c.Peers)
}

// run makes next the host's state: the ruleset first, then the tunnel. The ruleset is one `nft -f`
// transaction, which the kernel commits whole or not at all, so a refusal of it leaves the host as
// it was. A refusal of the tunnel, which follows a committed ruleset, puts the host back.
func (m *Manager) run(ctx context.Context, next Config) error {
	prev := m.c
	if committed, err := m.push(ctx, m.withBase(next)); err != nil {
		if committed {
			return m.undo(ctx, prev, err)
		}
		return err
	}
	m.c, m.ran = m.withBase(next), true
	return nil
}

func (m *Manager) withBase(c Config) Config {
	c.Management, c.TapPrefix, c.Egress, c.LAN, c.Proxy = m.Base.Management, m.Base.TapPrefix, m.Base.Egress, m.Base.LAN, m.Base.Proxy
	return c
}

// push writes c to the host: the ruleset, then the tunnel's address and peers. committed is whether the ruleset
// was committed, so that a failure of the tunnel is known to have changed the host.
func (m *Manager) push(ctx context.Context, c Config) (committed bool, err error) {
	c.At = m.now()
	if _, err := m.Run.Run(ctx, []byte(Ruleset(c)), "nft", "-f", "-"); err != nil {
		return false, fmt.Errorf("apply the ruleset: %w", err)
	}
	if m.Tunnel != nil {
		if err := m.Tunnel.Set(c.Mesh, c.Peers); err != nil {
			return true, fmt.Errorf("set the tunnel's peers: %w", err)
		}
	}
	return true, nil
}

// undo puts the host back to prev after cause stopped a change that had reached it, and returns
// cause. When the host refuses prev as well, what it holds is unknown: the next change is applied
// even if it equals prev, and the error says both failed.
func (m *Manager) undo(ctx context.Context, prev Config, cause error) error {
	m.c = prev
	if _, err := m.push(ctx, prev); err != nil {
		m.ran = false
		return errors.Join(cause, fmt.Errorf("roll back to the previous state: %w", err))
	}
	m.ran = true
	return cause
}

// apply makes next the state when the host accepted it and State holds it. A failure leaves the
// state as it was, on the host and in State.
func (m *Manager) apply(ctx context.Context, next Config) error {
	prev := m.c
	// Orch declares its networks and peers again every few seconds. A declaration that changes nothing
	// replaces nothing: the table is replaced whole, which would reset the counters of its drop rules.
	if m.ran && stateOf(next) == stateOf(prev) {
		// A renewal changes a deadline, which no rule holds; the file does.
		if sameDeadlines(next.Exposures, prev.Exposures) {
			return nil
		}
		// The set the rules match keeps each address for the time it has left; a renewal sets it again.
		if err := m.refresh(ctx, next); err != nil {
			return err
		}
		m.c.Exposures = next.Exposures
		return m.save()
	}
	if err := m.run(ctx, next); err != nil {
		return err
	}
	if err := m.save(); err != nil {
		return m.undo(ctx, prev, err)
	}
	return nil
}

// refresh sets the leased addresses' time left in the host's table, which the renewal changed, and
// nothing else.
func (m *Manager) refresh(ctx context.Context, c Config) error {
	c.At = m.now()
	c = m.withBase(c)
	if _, err := m.Run.Run(ctx, []byte(leasedRefresh(c)), "nft", "-f", "-"); err != nil {
		return fmt.Errorf("renew the leased addresses: %w", err)
	}
	return nil
}

// save writes the state to State.
func (m *Manager) save() error {
	if m.State == "" {
		return nil
	}
	boot, up, berr := m.boot()
	st := state{Networks: sortedNetworks(m.c.Networks), Members: m.c.Members, Peers: sortedPeers(m.c.Peers), Mesh: m.c.Mesh, MeshPool: m.c.MeshPool, Boot: boot}
	for _, e := range sortedExposures(m.c.Exposures) {
		st.Exposures = append(st.Exposures, leaseState{Exposure: e, Until: up + e.Deadline.Sub(m.now())})
	}
	if berr != nil {
		// Without the boot's clock no lease can be restored, so none is kept.
		st.Boot, st.Exposures = "", nil
	}
	raw, err := json.Marshal(st)
	if err == nil {
		err = writeFile(m.State, raw)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", m.State, err)
	}
	return nil
}

// sameDeadlines reports whether two lists of exposures that make the same rules also hold the same
// deadlines.
func sameDeadlines(a, b []Exposure) bool {
	if len(a) != len(b) {
		return false
	}
	at := map[netip.Addr]time.Time{}
	for _, e := range a {
		at[e.Address] = e.Deadline
	}
	for _, e := range b {
		if d, ok := at[e.Address]; !ok || !d.Equal(e.Deadline) {
			return false
		}
	}
	return true
}

// stateOf is the state of c as a canonical string: equal for two configurations that make the same
// ruleset and the same peers, whatever the order of their lists.
func stateOf(c Config) string {
	nets, peers := sortedNetworks(c.Networks), sortedPeers(c.Peers)
	for i := range nets {
		nets[i].Remote = sortedPrefixes(nets[i].Remote)
	}
	for i := range peers {
		peers[i].Prefixes = sortedPrefixes(peers[i].Prefixes)
	}
	members := append([]Member(nil), c.Members...)
	sort.Slice(members, func(i, j int) bool { return members[i].Address.Less(members[j].Address) })
	exposures := sortedExposures(c.Exposures)
	for i := range exposures {
		exposures[i].Deadline = time.Time{}
	}
	raw, _ := json.Marshal(struct {
		state
		Exposures []Exposure
		Machines  []netip.Prefix
	}{state{Networks: nets, Members: members, Peers: peers, Mesh: c.Mesh, MeshPool: c.MeshPool}, exposures, c.machines()})
	return string(raw)
}
