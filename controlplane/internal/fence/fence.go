// Package fence writes the netfilter rules that close etcd's client and peer ports to every
// address but the control-plane Nodes' (ADR 0024).
package fence

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// TableName is the table the rules live in. Applying replaces its rules and touches no other table.
const TableName = "naru-controlplane"

// ClientPort is etcd's client port; Ports are it and the peer port, the ones the rules close.
const ClientPort = 2379

var Ports = []uint16{ClientPort, 2380}

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

// Apply replaces the table's rules with: a TCP connection to ClientPort from an address inside one
// of machines is accepted; a TCP connection to one of Ports from an address that is not in allowed
// is rejected with a TCP reset. Everything else is accepted. machines are this Node's MicroVM
// prefix: a MicroVM's connection to another member leaves the Node masqueraded as the Node's
// address, which is allowed, but one to the Node's own member is local delivery and keeps the
// MicroVM's own address as its source.
func Apply(c *nftables.Conn, allowed []netip.Addr, machines []netip.Prefix) error {
	table := c.AddTable(&nftables.Table{Family: nftables.TableFamilyIPv4, Name: TableName})
	c.FlushTable(table)
	policy := nftables.ChainPolicyAccept
	chain := c.AddChain(&nftables.Chain{
		Name: "input", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &policy,
	})
	for _, m := range machines {
		if !m.Addr().Is4() {
			return fmt.Errorf("%s is not an IPv4 prefix", m)
		}
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: []expr.Any{
			&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.IPPROTO_TCP}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{byte(ClientPort >> 8), byte(ClientPort & 0xff)}},
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: net.CIDRMask(m.Bits(), 32), Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: m.Masked().Addr().AsSlice()},
			&expr.Verdict{Kind: expr.VerdictAccept},
		}})
	}
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
