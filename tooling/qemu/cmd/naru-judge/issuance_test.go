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

// issuingLog is node-1's gaon log with init's issuing lines, minus the one named, before the first
// service starts, as init writes them.
func issuingLog(t *testing.T, without string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range judge.Issuance(judge.GaonIssued...) {
		if !e.Absent && e.What != without {
			b.WriteString(strings.Join(e.Fragments, " ") + "\r\n")
		}
	}
	rest, err := os.ReadFile(logOf(t, judge.Gaon("node-1", true, 1), ""))
	require.NoError(t, err)
	b.Write(rest)
	p := filepath.Join(t.TempDir(), "serial.log")
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return p
}

func TestRun_WithGaonALogThatShowsInitIssuingIsJudgedOnItsIssuing(t *testing.T) {
	var out, errOut bytes.Buffer
	n2 := logOf(t, judge.Gaon("node-2", false, 1), "")

	assert.Equal(t, 0, run([]string{"-gaon", "-node1", issuingLog(t, ""), "-node2", n2}, &out, &errOut), errOut.String())

	errOut.Reset()
	assert.Equal(t, 1, run([]string{"-gaon", "-node1", issuingLog(t, "init issued scheduler"), "-node2", n2}, &out, &errOut))
	assert.Contains(t, errOut.String(), "FAIL node-1: init issued scheduler")
}
