package layout_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/naructl/internal/layout"
	"github.com/garamsh/naru/naructl/internal/pki"
)

const (
	tokenA = "abcdef.0123456789abcdef"
	tokenB = "ghijkl.fedcba9876543210"
)

func node(t *testing.T, change func(*layout.Node)) layout.Node {
	t.Helper()
	ca, err := pki.New("test")
	require.NoError(t, err)
	d := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	f := layout.File{Path: "/x", SHA256: d}
	n := layout.Node{
		Name: "node-1", Address: netip.MustParsePrefix("192.168.0.10/24"), APIServer: netip.MustParseAddr("192.168.0.10"),
		MachinePrefix: netip.MustParsePrefix("10.200.1.0/24"), CA: ca,
		Guest: layout.Manifest{
			Kernels: map[string]layout.File{"naru": f}, DefaultKernel: "naru", GuestInitrd: f, ConverterInitrd: f,
		},
		FirecrackerSHA256: d, Token: tokenA,
	}
	change(&n)
	return n
}

func TestUserData_RefusesATokenTheNodeCannotUse(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*layout.Node)
		want   string
	}{
		"no token":                       {func(n *layout.Node) { n.Token = "" }, "no bootstrap token"},
		"a token not in gaon's form":     {func(n *layout.Node) { n.Token = "short" }, "not <id>.<secret>"},
		"a list on a worker":             {func(n *layout.Node) { n.Tokens = []string{tokenA} }, "only the control-plane Node lists tokens"},
		"a control plane without a list": {func(n *layout.Node) { n.ControlPlane = true }, "does not hold its own token"},
		"a control plane whose list lacks its token": {func(n *layout.Node) {
			n.ControlPlane, n.Tokens = true, []string{tokenB}
		}, "does not hold its own token"},
		"a malformed token in the list": {func(n *layout.Node) {
			n.ControlPlane, n.Tokens = true, []string{tokenA, "nope"}
		}, "a token in the list"},
		"controllers disabled on a signer": {func(n *layout.Node) {
			n.ControlPlane, n.Tokens, n.Disable = true, []string{tokenA}, []string{"gaon-controllers"}
		}, "signs the agents' requests"},
		"a token with the pairs as files": {func(n *layout.Node) { n.PairsAsFiles = true }, "no use for a bootstrap token"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := node(t, c.change).UserData()
			assert.ErrorContains(t, err, c.want)
		})
	}
}

func TestUserData_PairsAsFilesTakesNoTokenAndAControlPlaneWithOneTakesNoSigner(t *testing.T) {
	data, err := node(t, func(n *layout.Node) { n.ControlPlane, n.PairsAsFiles, n.Token = true, true, "" }).UserData()
	require.NoError(t, err)

	assert.NotContains(t, string(data), "bootstrap-token")
	assert.NotContains(t, string(data), "--signing")
	assert.NotContains(t, string(data), "pki:")
}
