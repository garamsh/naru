package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
)

// issuingLog is node-1's gaon log, on a Node with interfaces interfaces, with init's issuing lines,
// minus the one named, before the first service starts, as init writes them.
func issuingLog(t *testing.T, interfaces int, without string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range judge.Issuance(judge.GaonIssued...) {
		if !e.Absent && e.What != without {
			b.WriteString(strings.Join(e.Fragments, " ") + "\r\n")
		}
	}
	rest, err := os.ReadFile(logOf(t, judge.Gaon(topo, "node-1", true, interfaces), ""))
	require.NoError(t, err)
	b.Write(rest)
	p := filepath.Join(t.TempDir(), "serial.log")
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return p
}

func TestRun_WithPKIANodeThatIssuedIsJudgedOnItsIssuing(t *testing.T) {
	var out, errOut bytes.Buffer
	n2 := logOf(t, judge.Gaon(topo, "node-2", false, 1), "")

	assert.Equal(t, 0, run([]string{"-gaon", "-naructl", naructlStub(t), "-pki", "-node1", issuingLog(t, 1, ""), "-node2", n2}, &out, &errOut), errOut.String())

	errOut.Reset()
	assert.Equal(t, 1, run([]string{"-gaon", "-naructl", naructlStub(t), "-pki", "-node1", issuingLog(t, 1, "init issued scheduler"), "-node2", n2}, &out, &errOut))
	assert.Contains(t, errOut.String(), "FAIL node-1: init issued scheduler")
}

func TestRun_WithPKIALogThatShowsNoIssuingFails(t *testing.T) {
	// The control: the same log, which carries its pairs as files, is accepted without -pki.
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Gaon(topo, "node-1", true, 1), "")
	n2 := logOf(t, judge.Gaon(topo, "node-2", false, 1), "")
	require.Equal(t, 0, run([]string{"-gaon", "-naructl", naructlStub(t), "-node1", n1, "-node2", n2}, &out, &errOut), errOut.String())

	errOut.Reset()
	assert.Equal(t, 1, run([]string{"-gaon", "-naructl", naructlStub(t), "-pki", "-node1", n1, "-node2", n2}, &out, &errOut))
	assert.Contains(t, errOut.String(), "FAIL node-1: init wrote the authority")
}

func TestRun_WithNode1InterfacesTheLogOfANodeWithThatManyIsAccepted(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Gaon(topo, "node-1", true, 2), "")
	n2 := logOf(t, judge.Gaon(topo, "node-2", false, 1), "")

	assert.Equal(t, 0, run([]string{"-gaon", "-naructl", naructlStub(t), "-node1-interfaces", "2", "-node1", n1, "-node2", n2}, &out, &errOut), errOut.String())

	errOut.Reset()
	assert.Equal(t, 1, run([]string{"-gaon", "-naructl", naructlStub(t), "-node1", n1, "-node2", n2}, &out, &errOut), "the control: one interface is expected by default")
	assert.Contains(t, errOut.String(), "FAIL node-1: init configured the network")
}

func TestRun_PKIAndNode1InterfacesWithoutGaonExitTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Node("node-1", true, true), "")
	n2 := logOf(t, judge.Node("node-2", false, false), "")
	require.Equal(t, 0, run([]string{"-node1", n1, "-node2", n2}, &out, &errOut), "the control: the same logs are accepted without them")

	assert.Equal(t, 2, run([]string{"-pki", "-node1", n1, "-node2", n2}, &out, &errOut))
	assert.Equal(t, 2, run([]string{"-node1-interfaces", "2", "-node1", n1, "-node2", n2}, &out, &errOut))
}
