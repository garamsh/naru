package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// topo is the topology the stub naructl prints.
var topo = naructl.Topology{
	Services: []naructl.Service{
		{Name: "registry"}, {Name: "etcd", ControlPlane: true},
		{Name: "gaon-apiserver", ControlPlane: true, Gaon: true}, {Name: "gaon-scheduler", ControlPlane: true, Gaon: true},
		{Name: "gaon-controllers", ControlPlane: true, Gaon: true}, {Name: "gaon-node-agent", Gaon: true},
	},
	Signer: "gaon-controllers", APIServer: "gaon-apiserver", Agent: "gaon-node-agent",
}

// naructlStub is an executable that prints topo as naructl topology does.
func naructlStub(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(topo)
	require.NoError(t, err)
	bin := filepath.Join(t.TempDir(), "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1\" = topology ] && echo '"+string(b)+"'\n"), 0o755)) //nolint:gosec // a test executable
	return bin
}

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
	n1 := logOf(t, judge.Node("node-1", true, true), "")
	n2 := logOf(t, judge.Node("node-2", false, false), "")

	code := run([]string{"-node1", n1, "-node2", n2}, &out, &errOut)

	assert.Equal(t, 0, code, errOut.String())
	assert.Contains(t, out.String(), "both Nodes show every expectation")
}

func TestRun_ALogMissingOneExpectationExitsOneAndNamesIt(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Node("node-1", true, true), "the probe finished")
	n2 := logOf(t, judge.Node("node-2", false, false), "")

	code := run([]string{"-node1", n1, "-node2", n2}, &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "FAIL node-1: the probe finished")
	assert.Equal(t, 1, strings.Count(errOut.String(), "FAIL "))
}

func TestRun_GaonNeedsANaructlAndNaructlNeedsGaon(t *testing.T) {
	var out, errOut bytes.Buffer
	n := logOf(t, judge.Gaon(topo, "node-1", true, 1), "")

	assert.Equal(t, 2, run([]string{"-gaon", "-node1", n, "-node2", n}, &out, &errOut))
	assert.Contains(t, errOut.String(), "-gaon needs -naructl")
	assert.Equal(t, 2, run([]string{"-naructl", naructlStub(t), "-node1", n, "-node2", n}, &out, &errOut))
	assert.Equal(t, 2, run([]string{"-gaon", "-naructl", "/nonexistent", "-node1", n, "-node2", n}, &out, &errOut), "a naructl that cannot be run is a usage error, not a judged log")
}

func TestRun_AServiceOfTheNaructlsTopologyThatTheLogLacksIsJudged(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Gaon(topo, "node-1", true, 1), "")
	n2 := logOf(t, judge.Gaon(topo, "node-2", false, 1), "")
	extended := topo
	extended.Services = append(slices.Clone(topo.Services), naructl.Service{Name: "gaon-new", Gaon: true})
	b, err := json.Marshal(extended)
	require.NoError(t, err)
	bin := filepath.Join(t.TempDir(), "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\n[ \"$1\" = topology ] && echo '"+string(b)+"'\n"), 0o755)) //nolint:gosec // a test executable

	code := run([]string{"-gaon", "-naructl", bin, "-node1", n1, "-node2", n2}, &out, &errOut)

	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "FAIL node-1: init started gaon-new")
	assert.Contains(t, errOut.String(), "FAIL node-2: init started gaon-new")
}

func TestRun_AMissingLogExitsTwo(t *testing.T) {
	var out, errOut bytes.Buffer

	assert.Equal(t, 2, run([]string{"-node1", "/nonexistent", "-node2", "/nonexistent"}, &out, &errOut))
	assert.Equal(t, 2, run(nil, &out, &errOut))
}

func TestRun_WithGaonTwoLogsThatShowGaonsServicesExitZeroAndOneWithoutTheRegistryExitsOne(t *testing.T) {
	var out, errOut bytes.Buffer
	n1 := logOf(t, judge.Gaon(topo, "node-1", true, 1), "")
	n2 := logOf(t, judge.Gaon(topo, "node-2", false, 1), "")

	assert.Equal(t, 0, run([]string{"-gaon", "-naructl", naructlStub(t), "-node1", n1, "-node2", n2}, &out, &errOut), errOut.String())
	assert.Equal(t, 1, run([]string{"-node1", n1, "-node2", n2}, &out, &errOut), "without -gaon the stand-in is expected")

	errOut.Reset()
	n2 = logOf(t, judge.Gaon(topo, "node-2", false, 1), "init started registry")
	assert.Equal(t, 1, run([]string{"-gaon", "-naructl", naructlStub(t), "-node1", n1, "-node2", n2}, &out, &errOut))
	assert.Contains(t, errOut.String(), "FAIL node-2: init started registry")
}
