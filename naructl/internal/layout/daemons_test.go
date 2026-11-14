package layout_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/naructl/internal/layout"
)

func TestCheck_RefusesTheDaemonSettingsInitWouldRefuse(t *testing.T) {
	_, ca := hostNode(t)
	join := &layout.SignerJoin{Peer: "192.168.0.41:7600", PeerCA: "ca", Authorisation: "{}"}
	ext := []layout.Extension{{Name: layout.OrchExtension, SHA256: strings.Repeat("e", 64)}}
	tests := []struct {
		name   string
		mutate func(*layout.Node)
		want   string
	}{
		{"signer without the orch extension", func(n *layout.Node) { n.Extensions, n.Signer = nil, &layout.Signer{} }, "run from the naru-orch extension"},
		{"extension without a name", func(n *layout.Node) { n.Extensions = []layout.Extension{{SHA256: strings.Repeat("e", 64)}} }, "needs a name"},
		{"extension with an upper-case digest", func(n *layout.Node) {
			n.Extensions = []layout.Extension{{Name: "x", SHA256: strings.Repeat("E", 64)}}
		}, "lowercase hex SHA-256"},
		{"extension given twice", func(n *layout.Node) { n.Extensions = append(append([]layout.Extension{}, ext...), ext...) }, "given twice"},
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
			n := node(t, func(n *layout.Node) {
				n.Extensions = ext
				tt.mutate(n)
			})
			err := n.Check()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
