//go:build qemu_real

package tests_test

import (
	"crypto/tls"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The serial port of a Node is a console the guest pays for in time, whoever reads it (ADR 0036). This
// test boots three control-plane Nodes whose serial ports are unix sockets nobody reads, as a Proxmox
// `serial0: socket` is, and watches them for stallWindow after they have run for settle: every Node
// answers a TLS handshake within maxStall each time it is tried, and the holder of the address never
// releases it.
const (
	// settle is how long the Nodes run before the window opens, which is long enough for several
	// re-prints of the console log and for the control plane to have formed.
	settle = 60 * time.Second
	// stallWindow is the time the Nodes are probed, 5 times a second each.
	stallWindow = 120 * time.Second
	// maxStall is the slowest handshake the Nodes may answer with. Before ADR 0036 a re-print of the
	// console log held a Node for 0.3 to 1.5 s in this window.
	maxStall = 100 * time.Millisecond
)

func TestControlPlane_ThreeNodesWhoseSerialSocketNobodyReadsAnswerWithinAHundredMillisecondsAndTheHolderNeverReleasesTheAddress(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 3, Gaon: true, BootPKI: true, ControlPlanes: 3, NodeAPI: true, SerialSocket: true})
	eventually(t, "node-1 Node API", func() bool { return ctl(c.API.Admin, "version").Err == nil })
	eventually(t, "vip running", func() bool { return pids(t, c.API.Admin)["vip"] != "" })
	time.Sleep(settle)
	before := ctl(c.API.Admin, "logs", "vip").Stdout

	var mu sync.Mutex
	slow := map[string][]string{}
	probed := map[string]int{}
	began := time.Now()
	var wg sync.WaitGroup
	for name, addr := range c.HostAddrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(200 * time.Millisecond)
			defer tick.Stop()
			for time.Since(began) < stallWindow {
				<-tick.C
				at := time.Now()
				d := net.Dialer{Timeout: 30 * time.Second}
				conn, err := tls.DialWithDialer(&d, "tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // only the time the handshake takes is measured
				took := time.Since(at)
				if err == nil {
					_ = conn.Close()
				}
				mu.Lock()
				probed[name]++
				if err != nil || took > maxStall {
					slow[name] = append(slow[name], at.Sub(began).Round(time.Millisecond).String()+" took "+took.Round(time.Millisecond).String())
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	after := ctl(c.API.Admin, "logs", "vip").Stdout

	names := make([]string, 0, len(probed))
	for name := range probed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		assert.Greater(t, probed[name], 500, name+" was probed through the window")
		assert.Empty(t, slow[name], name+" answered slower than "+maxStall.String()+" at")
	}
	assert.Equal(t, strings.Count(before, "released"), strings.Count(after, "released"), "node-1's vip released the address in the window:\n%s", after)

	// What probes the API server leaves it nothing to log: no service's log has a handshake error.
	for service := range pids(t, c.API.Admin) {
		logs := ctl(c.API.Admin, "logs", service)
		require.NoError(t, logs.Err, service)
		assert.NotContains(t, logs.Stdout, "TLS handshake error", service)
	}
}
