package netecho

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleRoutes = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	FE00A8C0	0003	0	0	100	00000000	0	0	0
eth0	0000A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
eth0	00C8730A	0200000A	0003	0	0	0	00FFFFFF	0	0	0
`

func TestGatewayRoutes_ListsTheRoutesThroughAGatewayButNotTheDefault(t *testing.T) {
	assert.Equal(t, []string{"10.115.200.0/24 via 10.0.0.2"}, gatewayRoutes(sampleRoutes))
}

func TestGatewayRoutes_AnEmptyTableHasNone(t *testing.T) {
	assert.Empty(t, gatewayRoutes(""))
}

// lockedBuffer is a log sink the test reads while the service writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestRun_TheReceiverReportsTheSourceAddressASenderGives(t *testing.T) {
	// The loopback delivers a datagram from 127.0.0.2 to 127.0.0.1 with its source unchanged.
	probe, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.LocalAddr().String()
	require.NoError(t, probe.Close())
	ctx, cancel := context.WithCancel(context.Background())
	var out lockedBuffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Listen: []string{addr}, Send: []string{"127.0.0.2=" + addr}}, slog.New(slog.NewTextHandler(&out, nil)))
	}()

	require.Eventually(t, func() bool { return strings.Contains(out.String(), "received from=127.0.0.2 to=127.0.0.1") }, 5*time.Second, 50*time.Millisecond, out.String())
	cancel()
	require.NoError(t, <-done)
}

func TestRun_ASendWithoutASourceIsRefused(t *testing.T) {
	err := Run(context.Background(), Options{Send: []string{"127.0.0.1:9"}}, slog.New(slog.NewTextHandler(&lockedBuffer{}, nil)))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "<from>=<to>")
}
