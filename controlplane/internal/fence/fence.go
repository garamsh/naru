// Package fence writes the netfilter rules that close etcd's client and peer ports to every
// address but the control-plane Nodes' (ADR 0024).
package fence

import (
	"errors"
	"fmt"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// TableName is the table the rules live in. Applying replaces its rules and touches no other table.
const TableName = "naru-controlplane"

// Ports are etcd's client and peer ports, the ones the rules close.
var Ports = []uint16{2379, 2380}

// Allowed is the addresses that may reach Ports, once each, or an error for an address that is not
// IPv4: etcd listens on IPv4 addresses only.
func Allowed(addrs []netip.Addr) ([]netip.Addr, error) {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, a := range addrs {
		if !a.Is4() {
			return nil, fmt.Errorf("%s is not an IPv4 address", a)
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no address is allowed")
	}
	return out, nil
}

// Apply replaces the table's rules with: a TCP connection to one of Ports from an address that is
// not in allowed is rejected with a TCP reset. Everything else is accepted.
func Apply(c *nftables.Conn, allowed []netip.Addr) error {
	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: TableName})
	c.FlushTable(table)
	policy := nftables.ChainPolicyAccept
	chain := c.AddChain(&nftables.Chain{
		Name: "input", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &policy,
	})
	for _, port := range Ports {
		set := &nftables.Set{Table: table, Anonymous: true, Constant: true, KeyType: nftables.TypeIPAddr}
		elems := make([]nftables.SetElement, len(allowed))
		for i, a := range allowed {
			elems[i] = nftables.SetElement{Key: a.AsSlice()}
		}
		if err := c.AddSet(set, elems); err != nil {
			return fmt.Errorf("the set of allowed addresses: %w", err)
		}
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(port >> 8), byte(port)}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
			&expr.Lookup{SourceRegister: 1, SetName: set.Name, SetID: set.ID, Invert: true},
			&expr.Reject{Type: unix.NFT_REJECT_TCP_RST},
		}})
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("write the rules: %w", err)
	}
	return nil
}
