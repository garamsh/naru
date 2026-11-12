package judge_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// consoleOf is a console that shows every expectation: a line holding each of the fragments of
// each one that is not absent.
func consoleOf(want []judge.Expect) string {
	var b strings.Builder
	for _, e := range want {
		if !e.Absent {
			b.WriteString("time=x level=INFO msg=" + strings.Join(e.Fragments, " x ") + "\n")
		}
	}
	return b.String()
}

func TestCheck_AConsoleWithEveryExpectationPasses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		controlPlane bool
		probe        bool
	}{{"node-1", true, true}, {"node-2", false, false}} {
		want := judge.Node(tc.name, tc.controlPlane, tc.probe)

		failures := judge.Check(tc.name, judge.Clean([]byte(consoleOf(want))), want)

		assert.Empty(t, failures, tc.name)
	}
}

func TestCheck_EachMissingExpectationIsNamedAlone(t *testing.T) {
	want := judge.Node("node-1", true, true)
	for i, gone := range want {
		if gone.Absent {
			continue
		}
		kept := append(append([]judge.Expect{}, want[:i]...), want[i+1:]...)

		failures := judge.Check("node-1", judge.Clean([]byte(consoleOf(kept))), want)

		require.Len(t, failures, 1, gone.What)
		assert.Equal(t, gone.What, failures[0].Expect.What)
		assert.Contains(t, failures[0].String(), "node-1: "+gone.What)
	}
}

func TestCheck_AFailedProbeLineFailsTheNode(t *testing.T) {
	want := judge.Node("node-1", true, true)
	console := consoleOf(want) + "PROBE machine-still-running fail the machine is gone\n"

	failures := judge.Check("node-1", judge.Clean([]byte(console)), want)

	require.Len(t, failures, 1)
	assert.Equal(t, "no probe check failed", failures[0].Expect.What)
}

func TestCheck_TheOtherNodeMustNotHaveStartedTheControlService(t *testing.T) {
	want := judge.Node("node-2", false, false)
	console := consoleOf(want) + "time=x msg=started service=control\n"

	failures := judge.Check("node-2", judge.Clean([]byte(console)), want)

	require.Len(t, failures, 1)
	assert.Equal(t, "init never started the control service", failures[0].Expect.What)
}

func TestCheck_TheNameInTheConfigurationLineIsTheNodes(t *testing.T) {
	want := judge.Node("node-1", true, false)

	failures := judge.Check("node-1", judge.Clean([]byte(consoleOf(judge.Node("node-2", true, false)))), want)

	require.Len(t, failures, 1)
	assert.Equal(t, "init accepted the configuration", failures[0].Expect.What)
}

func TestCheck_TheNetworkLineMustCountOneInterfaceAndAnyNumberOfNameservers(t *testing.T) {
	want := judge.Boot("node-1")
	line := func(msg, counts string) string {
		var others []judge.Expect
		for _, e := range want {
			if e.What != "init configured the network" {
				others = append(others, e)
			}
		}
		return consoleOf(others) + "time=x level=INFO msg=\"" + msg + "\" " + counts + "\n"
	}
	for _, tc := range []struct {
		name, console string
		failed        bool
	}{
		{"one nameserver", line("network configured", "interfaces=1 nameservers=1"), false},
		{"two nameservers", line("network configured", "interfaces=1 nameservers=2"), false},
		{"no nameserver count", line("network configured", "interfaces=1"), false},
		{"two interfaces", line("network configured", "interfaces=2 nameservers=2"), true},
		{"no interface", line("network configured", "interfaces=0 nameservers=2"), true},
		{"no network line", line("network ignored", "interfaces=1 nameservers=1"), true},
	} {
		failures := judge.Check("node-1", judge.Clean([]byte(tc.console)), want)

		if tc.failed {
			require.Len(t, failures, 1, tc.name)
			assert.Equal(t, "init configured the network", failures[0].Expect.What, tc.name)
		} else {
			assert.Empty(t, failures, tc.name)
		}
	}
}

func TestClean_ReadsACaptureFromATerminal(t *testing.T) {
	raw := "ial line cut by the capture\r\n" +
		"\x1b[0;32mtime=x msg=\"network configured\"\x1b[0m interfaces=1\r\n" +
		"\x1b]0;title\x07progress\rdone\n"

	l := judge.Clean([]byte(raw))

	assert.True(t, judge.HasLine(l.Text, []string{"network configured", "interfaces=1"}), "escapes and CR LF are gone")
	assert.True(t, judge.HasLine(l.Text, []string{"ial line cut by the capture"}), "a first line cut short is kept")
	assert.True(t, judge.HasLine(l.Text, []string{"done"}), "a lone CR ends a line")
	assert.NotContains(t, l.Text, "\x1b")
	assert.NotContains(t, l.Text, "\r")
}

func TestClean_RemovesTheReprintMarkersAndCountsThem(t *testing.T) {
	raw := "boot line\n" +
		"NARU-LOG-BEGIN seq=1 lines=1 dropped=0\nboot line\nNARU-LOG-END seq=1\n" +
		"\x1b[0mNARU-LOG-BEGIN seq=2 lines=2 dropped=7\r\nboot line\nlater line\nNARU-LOG-END seq=2\n"

	l := judge.Clean([]byte(raw))

	assert.NotContains(t, l.Text, "NARU-LOG")
	assert.Equal(t, 2, l.Reprints)
	assert.Equal(t, 7, l.Dropped)
	assert.Equal(t, 4, strings.Count(l.Text, "boot line\n")+strings.Count(l.Text, "later line\n"), "the lines stay, as many times as they were printed")
}

func TestCheck_ALogOfReprintedBlocksJudgesAsTheLogDoes(t *testing.T) {
	want := judge.Node("node-1", true, true)
	one := consoleOf(want)
	var many strings.Builder
	for i := 1; i <= 20; i++ {
		many.WriteString("NARU-LOG-BEGIN seq=1 lines=1 dropped=0\n" + one + "NARU-LOG-END seq=1\n")
	}

	assert.Empty(t, judge.Check("node-1", judge.Clean([]byte(many.String())), want))
}

func TestHasLine_FragmentsMustShareALineInOrder(t *testing.T) {
	console := "configuration accepted node=a\nservice=mvmd started\n"

	assert.True(t, judge.HasLine(console, []string{"configuration accepted", "node=a"}))
	assert.False(t, judge.HasLine(console, []string{"started", "service=mvmd"}), "in order")
	assert.False(t, judge.HasLine(console, []string{"configuration accepted", "started"}), "on one line")
}

func TestGaon_ANodeShowsTheRegistryAndTheNodeAgentAndTheControlPlaneOnlyOnNodeOne(t *testing.T) {
	for _, tc := range []struct {
		name         string
		controlPlane bool
	}{{"node-1", true}, {"node-2", false}} {
		want := judge.Gaon(topo, tc.name, tc.controlPlane, 1)
		assert.Empty(t, judge.Check(tc.name, judge.Clean([]byte(consoleOf(want))), want), tc.name)
	}

	second := judge.Gaon(topo, "node-2", false, 1)
	console := consoleOf(second) + "time=x msg=started service=gaon-apiserver\n"
	failures := judge.Check("node-2", judge.Clean([]byte(console)), second)
	require.Len(t, failures, 1)
	assert.Equal(t, "init never started gaon-apiserver", failures[0].Expect.What)

	// The registry runs on every Node: a Node whose console lacks its start fails, on either role.
	for _, tc := range []struct {
		name         string
		controlPlane bool
	}{{"node-1", true}, {"node-2", false}} {
		want := judge.Gaon(topo, tc.name, tc.controlPlane, 1)
		var without []judge.Expect
		for _, e := range want {
			if e.What != "init started registry" {
				without = append(without, e)
			}
		}
		require.Len(t, without, len(want)-1, "the expectation for the registry exists on "+tc.name)
		failures := judge.Check(tc.name, judge.Clean([]byte(consoleOf(without))), want)
		require.Len(t, failures, 1, tc.name)
		assert.Equal(t, "init started registry", failures[0].Expect.What)
	}

	first := judge.Gaon(topo, "node-1", true, 1)
	failures = judge.Check("node-1", judge.Clean([]byte(consoleOf(first[:len(first)-1]))), first)
	require.Len(t, failures, 1)
	assert.Equal(t, "init started gaon-node-agent", failures[0].Expect.What)
}

func TestGaon_JudgesEveryServiceTheTopologyNamesAndNoOtherAndDoesNotKnowTheNamesItself(t *testing.T) {
	// A layout that renamed a service and added one: the judge follows it with no change of its own.
	renamed := naructl.Topology{Services: []naructl.Service{
		{Name: "alpha"}, {Name: "beta", ControlPlane: true}, {Name: "gamma"},
	}, Signer: "beta", APIServer: "beta", Agent: "alpha"}
	for _, tc := range []struct {
		controlPlane bool
		started      []string
		skipped      []string
	}{
		{true, []string{"alpha", "beta", "gamma"}, nil},
		{false, []string{"alpha", "gamma"}, []string{"beta"}},
	} {
		var started, skipped []string
		for _, e := range judge.Gaon(renamed, "node-1", tc.controlPlane, 1) {
			if name, ok := strings.CutPrefix(e.What, "init started "); ok && name != "mvmd" {
				started = append(started, name)
			}
			if name, ok := strings.CutPrefix(e.What, "init skipped "); ok {
				skipped = append(skipped, name)
			}
		}
		assert.Equal(t, tc.started, started, "controlPlane=%v", tc.controlPlane)
		assert.Equal(t, tc.skipped, skipped, "controlPlane=%v", tc.controlPlane)
	}
}
