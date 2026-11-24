package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
)

func logOf(t *testing.T, want []judge.Expect, without string) string {
	t.Helper()
	var b strings.Builder
	for _, e := range want {
		if !e.Absent && e.What != without {
			b.WriteString(strings.Join(e.Fragments, " ") + "\r\n")
		}
	}
	p := filepath.Join(t.TempDir(), "serial.log")
	assert.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return p
}

func TestRun_TwoLogsThatShowEveryExpectationExitZero(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Node("node-1", true), "")
	n2 := logOf(t, judge.Node("node-2", false), "")

	code := run([]string{"-node1", n1, "-node2", n2}, &out, &errOut)

	assert.Equal(t, 0, code, errOut.String())
	assert.Contains(t, out.String(), "both Nodes show every expectation")
}

func TestRun_ALogMissingOneExpectationExitsOneAndNamesIt(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Node("node-1", true), "the probe finished")
	n2 := logOf(t, judge.Node("node-2", false), "")

	code := run([]string{"-node1", n1, "-node2", n2}, &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "FAIL node-1: the probe finished")
	assert.Equal(t, 1, strings.Count(errOut.String(), "FAIL "))
}

func TestRun_AMissingLogExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer

	assert.Equal(t, 2, run([]string{"-node1", "/nonexistent", "-node2", "/nonexistent"}, &out, &errOut))
	assert.Equal(t, 2, run(nil, &out, &errOut))
}

func TestRun_WithReleaseTwoLogsThatShowMvmdAndTheRegistryExitZeroAndOneWithoutTheRegistryExitsOne(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Released("node-1"), "")
	n2 := logOf(t, judge.Released("node-2"), "")

	assert.Equal(t, 0, run([]string{"-release", "-node1", n1, "-node2", n2}, &out, &errOut), errOut.String())
	assert.Equal(t, 1, run([]string{"-node1", n1, "-node2", n2}, &out, &errOut), "without -release the stand-in is expected")

	errOut.Reset()
	n2 = logOf(t, judge.Released("node-2"), "init started the registry")
	assert.Equal(t, 1, run([]string{"-release", "-node1", n1, "-node2", n2}, &out, &errOut))
	assert.Contains(t, errOut.String(), "FAIL node-2: init started the registry")
}

func TestRun_NodeAPIJudgesTheLineOfTheStateTheFlagNames(t *testing.T) {
	var out, errOut bytes.Buffer
	off := logOf(t, append(judge.Node("node-1", true), judge.NodeAPI(false)...), "")
	off2 := logOf(t, append(judge.Node("node-2", false), judge.NodeAPI(false)...), "")
	plain1 := logOf(t, judge.Node("node-1", true), "")
	plain2 := logOf(t, judge.Node("node-2", false), "")

	assert.Equal(t, 0, run([]string{"-node-api", "off", "-node1", off, "-node2", off2}, &out, &errOut), errOut.String())
	assert.Equal(t, 1, run([]string{"-node-api", "listening", "-node1", off, "-node2", off2}, &out, &errOut), "a Node that says the API is off does not serve it")
	assert.Equal(t, 1, run([]string{"-node-api", "off", "-node1", plain1, "-node2", plain2}, &out, &errOut), "no line is not the API off")
	assert.Equal(t, 0, run([]string{"-node1", plain1, "-node2", plain2}, &out, &errOut), "left out, the lines are not judged")
	assert.Equal(t, 2, run([]string{"-node-api", "on", "-node1", plain1, "-node2", plain2}, &out, &errOut))
}
