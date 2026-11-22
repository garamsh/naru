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
// answers every TLS handshake, none repeatedly slower than maxStall, and the holder of the address never
// releases it.
const (
	// settle is how long the Nodes run before the window opens, which is long enough for several
	// re-prints of the console log and for the control plane to have formed.
	settle = 90 * time.Second
	// stallWindow is the time the Nodes are probed, 5 times a second each, unless NARU_STALL_WINDOW
	// gives another (a Go duration, such as 30m).
	stallWindow = 120 * time.Second
	// maxStall and repeatStalls are what separates a stall from load, and the test fails a Node that
	// answers repeatStalls handshakes slower than maxStall, or one that fails a handshake. A guest the
	// serial port holds does not run while the device takes a write, at 3 to 15 ms a KiB (ADR 0036),
	// and the filler leaves 1 MiB kept: a re-print of all of it holds the guest 3 s at the least,
	// so a probe that is in flight when it starts or arrives during it waits 2.8 s at the least, and
	// it does so on every re-print of the Node, which is every 30 s, so at least 3 times in the window.
	// A Node whose re-print is the 16 KiB in 2 KiB pieces holds the guest 30 ms at the most per piece.
	// Load on a shared development host is the other cause of a slow handshake: under load averages of
	// 15 to 75, 1 to 6 of about 600 handshakes per Node took 1.0 to 2.1 s, and none longer, where at low
	// load the slowest took 285 ms. 2.5 s is 0.4 s above that load and 0.3 s below the shortest stall,
	// and a stall is told from a load spike beyond what was measured by its repeating: one handshake
	// slower than 2.5 s is not enough, two are. Neither distribution was measured on the host the test
	// runs on for this bound, the stall confirmed by a run with the whole 1 MiB kept log printed at
	// once (ReprintBytes = KeptBytes, BlockPiece = KeptBytes + 64<<10): node-3 stalled at 10.9 s, 8.9 s
	// and 11.0 s, node-1 at 13.4 s (garamsh/naru#235, comment 6939492640). With ReprintBytes = KeptBytes
	// alone the longest handshake was 69 ms: the block is still written in 2 KiB pieces. The 100 ms of
	// the criterion is aurora's on Proxmox.
	maxStall     = 2500 * time.Millisecond
	repeatStalls = 2
	// slowHandshake is the length past which a handshake is counted in the log, so that a run that
	// passed on a loaded host shows how loaded it was. It fails nothing.
	slowHandshake = time.Second
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
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", filepath.Join(inputs.Dir, "bin", "naru105-logfill"), "./tests/testdata/naru105-logfill")
	cmd.Dir = filepath.Join(os.Getenv("NARU_ROOT"), "tooling", "qemu")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return path.Join(fixture.GuestRoot, "bin/naru105-logfill")
}

func TestControlPlane_ThreeNodesWhoseSerialSocketNobodyReadsAnswerWithoutRepeatedStallsAndTheHolderNeverReleasesTheAddress(t *testing.T) {
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
	broken := map[string][]string{}
	probed := map[string]int{}
	slowest := map[string]time.Duration{}
	lagged := map[string]int{}
	laggedAt := map[string][]string{}
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
				conn, err := tls.DialWithDialer(&d, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
				took := time.Since(at)
				if err == nil {
					_ = conn.Close()
				}
				mu.Lock()
				probed[name]++
				slowest[name] = max(slowest[name], took)
				if took > slowHandshake {
					lagged[name]++
					laggedAt[name] = append(laggedAt[name], at.Sub(began).Round(100*time.Millisecond).String()+" "+took.Round(10*time.Millisecond).String())
				}
				if err != nil {
					broken[name] = append(broken[name], at.Sub(began).Round(time.Millisecond).String()+": "+err.Error())
				} else if took > maxStall {
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
		assert.Empty(t, broken[name], name+" failed a handshake at")
		assert.Less(t, len(slow[name]), repeatStalls, name+" answered slower than "+maxStall.String()+" at "+strings.Join(slow[name], ", "))
	}
	for _, name := range names {
		t.Logf("%s: %d handshakes in %s, slowest %s, %d slower than %s, %d slower than %s", name, probed[name], stall, slowest[name].Round(time.Millisecond), lagged[name], slowHandshake, len(slow[name]), maxStall)
		t.Logf("%s: handshakes slower than %s, at offset and length: %s", name, slowHandshake, strings.Join(laggedAt[name], ", "))
	}
	assert.Equal(t, strings.Count(before, "released"), strings.Count(after, "released"), "node-1's vip released the address in the window:\n%s", after)

	// What probes the API server leaves it nothing to log: no service's log has a handshake error.
	for service := range pids(t, c.API.Admin) {
		logs := ctl(c.API.Admin, "logs", service)
		require.NoError(t, logs.Err, service)
		assert.NotContains(t, logs.Stdout, "TLS handshake error", service)
	}
}
