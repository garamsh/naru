package judge_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
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
