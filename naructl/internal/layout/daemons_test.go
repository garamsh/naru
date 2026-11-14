package layout_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/naructl/internal/layout"
)

func TestCheck_RefusesTheDaemonSettingsInitWouldRefuse(t *testing.T) {
	_, ca := hostNode(t)
	join := &layout.SignerJoin{Peer: "192.168.0.41:7600", PeerCA: "ca", Authorisation: "{}"}
	tests := []struct {
		name   string
		mutate func(*layout.Node)
		want   string
	}{
		{"join without replica", func(n *layout.Node) { n.Signer = &layout.Signer{Join: join} }, "give --signer-replica"},
		{"join missing its authorisation", func(n *layout.Node) {
			n.Signer = &layout.Signer{Replica: true, Join: &layout.SignerJoin{Peer: "p:1", PeerCA: "ca"}}
		}, "joining needs"},
		{"replica beside a root request", func(n *layout.Node) {
			n.Signer = &layout.Signer{Replica: true, RecoveryPublicKey: "{}", RootCSR: "csr"}
		}, "not both"},
		{"enrollment missing its key", func(n *layout.Node) {
			n.Host = &layout.Host{CA: ca, Region: "eu-1"}
			n.HostOrch = &layout.HostOrch{URL: "https://o:7500", CASHA256: "a"}
		}, "enrollment key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := node(t, tt.mutate)
			err := n.Check()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
