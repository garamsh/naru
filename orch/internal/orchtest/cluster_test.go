package orchtest

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStart_ChoosesOtherPortsWhenAnotherSocketHoldsOneOfItsPorts(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = held.Close() })
	taken := held.Addr().(*net.TCPAddr).Port

	real, picks := pickPorts, 0
	pickPorts = func(t testing.TB, n int) []int {
		picks++
		ports := real(t, n)
		if picks == 1 {
			ports[0] = taken
		}
		return ports
	}
	t.Cleanup(func() { pickPorts = real })

	c := Start(t, Options{Members: 3})

	assert.Greater(t, picks, 1, "the first choice was refused, so the ports were chosen again")
	assert.Len(t, c.Members, 3)
	for _, m := range c.Members {
		assert.NotNil(t, m)
	}
}
