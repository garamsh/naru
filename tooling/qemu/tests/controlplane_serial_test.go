//go:build qemu_real

package tests_test

import (
	"crypto/tls"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
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
	settle = 90 * time.Second
	// stallWindow is the time the Nodes are probed, 5 times a second each, unless NARU_STALL_WINDOW
	// gives another (a Go duration, such as 30m).
	stallWindow = 120 * time.Second
	// maxStall is the slowest handshake the Nodes may answer with. The 100 ms of the criterion is
	// aurora's on Proxmox; on a shared development host nested QEMU alone takes a Node past it
	// (ADR 0036), and 1 s is above that and below the seconds a full re-print holds a Node.
	maxStall = time.Second
)

func window(t *testing.T) time.Duration {
	v := os.Getenv("NARU_STALL_WINDOW")
	if v == "" {
		return stallWindow
	}
	d, err := time.ParseDuration(v)
	require.NoError(t, err, "NARU_STALL_WINDOW")
	require.Positive(t, d, "NARU_STALL_WINDOW")
	return d
}

// buildLogFill builds the test-only service that fills a Node's kept console log into the staged inputs,
// which every Node's data disk carries, and returns its path in the guest.
func buildLogFill(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", filepath.Join(inputs.Dir, "bin", "naru105-logfill"), "./tests/testdata/naru105-logfill") //nolint:gosec // arguments are fixed here
	cmd.Dir = filepath.Join(os.Getenv("NARU_ROOT"), "tooling", "qemu")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return path.Join(fixture.GuestRoot, "bin/naru105-logfill")
}

func TestControlPlane_ThreeNodesWhoseSerialSocketNobodyReadsAnswerWithinASecondAndTheHolderNeverReleasesTheAddress(t *testing.T) {
	fill := node.ExtraService{Name: "naru105-logfill", Binary: buildLogFill(t)}
	c := start(t, node.ClusterSpec{N: 3, Gaon: true, BootPKI: true, ControlPlanes: 3, NodeAPI: true, SerialSocket: true, ExtraServices: []node.ExtraService{fill}})
	eventually(t, "node-1 Node API", func() bool { return ctl(c.API.Admin, "version").Err == nil })
	eventually(t, "vip running", func() bool { return pids(t, c.API.Admin)["vip"] != "" })
	stall := window(t)
	// The filler prints more than KeptBytes (1 MiB) of lines before the window opens, as an aurora
	// Node's log holds after a while, so a re-print of everything kept is 1 MiB.
	eventually(t, "the filler has filled the kept log", func() bool {
		return strings.Contains(ctl(c.API.Admin, "logs", fill.Name).Stdout, "naru105-logfill: done")
	})
	time.Sleep(settle)
	before := ctl(c.API.Admin, "logs", "vip").Stdout

	var mu sync.Mutex
	slow := map[string][]string{}
	probed := map[string]int{}
	slowest := map[string]time.Duration{}
	began := time.Now()
	var wg sync.WaitGroup
	for name, addr := range c.HostAddrs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(200 * time.Millisecond)
			defer tick.Stop()
			for time.Since(began) < stall {
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
				slowest[name] = max(slowest[name], took)
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
		assert.Greater(t, probed[name], int(stall/time.Second)*4, name+" was probed through the window")
		assert.Empty(t, slow[name], name+" answered slower than "+maxStall.String()+" at")
	}
	for _, name := range names {
		t.Logf("%s: %d handshakes in %s, slowest %s, %d slower than %s", name, probed[name], stall, slowest[name].Round(time.Millisecond), len(slow[name]), maxStall)
	}
	assert.Equal(t, strings.Count(before, "released"), strings.Count(after, "released"), "node-1's vip released the address in the window:\n%s", after)

	// What probes the API server leaves it nothing to log: no service's log has a handshake error.
	for service := range pids(t, c.API.Admin) {
		logs := ctl(c.API.Admin, "logs", service)
		require.NoError(t, logs.Err, service)
		assert.NotContains(t, logs.Stdout, "TLS handshake error", service)
	}
}
