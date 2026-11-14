//go:build qemu_real

package tests_test

import (
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The tests of a Node's state (ADR 0047): `naructl get config` prints one YAML document with the
// status of the Node beside its configuration, an apply naming the status is refused, and the Node
// has no login path.

// document is what `naructl get config` prints.
type document struct {
	Metadata struct{ Name string }
	Status   struct {
		Version     string
		BootID      string `yaml:"bootId"`
		Slot        string
		LastUpgrade *struct{ From, To, Result string } `yaml:"lastUpgrade"`
		Revision    int
		Services    []serviceStatus
	}
}

type serviceStatus struct {
	Name     string
	State    string
	Restarts int
}

// documentOf reads the one document `naructl get config` prints, and fails when it prints more.
func documentOf(t *testing.T, file string) document {
	t.Helper()
	r := ctl(file, "get", "config")
	require.NoError(t, r.Err, r.refusal())
	dec := yaml.NewDecoder(strings.NewReader(r.Stdout))
	var doc document
	require.NoError(t, dec.Decode(&doc), r.Stdout)
	var more yaml.Node
	require.ErrorIs(t, dec.Decode(&more), io.EOF, "the output is one document")
	return doc
}

func TestNode_GetConfigPrintsOneDocumentWhoseStatusIsTheVersionTheBootAndTheServices(t *testing.T) {
	c := nodeAPI(t)
	want := ""
	eventually(t, "every service of node-1 is running in the status", func() bool {
		doc := documentOf(t, c.API.Reader)
		want = doc.Status.Version
		return len(doc.Status.Services) > 0 && !slices.ContainsFunc(doc.Status.Services, func(s serviceStatus) bool { return s.State != "running" })
	})

	doc := documentOf(t, c.API.Reader)

	version, bootID := versionOf(t, c.API.Reader)
	assert.Equal(t, version, doc.Status.Version, "status.version is what `naructl version` prints")
	assert.Equal(t, want, doc.Status.Version)
	assert.Equal(t, bootID, doc.Status.BootID)
	assert.Equal(t, "default", doc.Status.Slot)
	assert.Nil(t, doc.Status.LastUpgrade, "no upgrade has happened")
	assert.Equal(t, 0, doc.Status.Revision, "the user-data is revision 0")
	assert.Equal(t, "naru-node-1", doc.Metadata.Name, "the configuration is in the same document")
	running := pids(t, c.API.Reader)
	for _, s := range doc.Status.Services {
		assert.Contains(t, running, s.Name, "the status lists what `naructl services` lists running")
	}
}

func TestNode_AnApplyNamingStatusIsRefusedWith422AndChangesNothing(t *testing.T) {
	c := nodeAPI(t)
	before := documentOf(t, c.API.Admin)

	r := ctl(c.API.Admin, "apply", "-f", patchFile(t, "status:\n  version: v9.9.9\n"))

	require.Error(t, r.Err)
	assert.Contains(t, r.refusal(), "422")
	assert.Contains(t, r.refusal(), "status")
	mixed := ctl(c.API.Admin, "apply", "-f", patchFile(t, idle("1m")+"status:\n  slot: trial\n"))
	require.Error(t, mixed.Err, "a patch that names status beside a valid change is refused whole")
	assert.Contains(t, mixed.refusal(), "422")
	after := documentOf(t, c.API.Admin)
	assert.Equal(t, before, after, "the revision and the status are as they were")

	control := ctl(c.API.Admin, "apply", "-f", patchFile(t, idle("1m")))
	require.NoError(t, control.Err, "the same change without status is applied: "+control.refusal())
	assert.Equal(t, before.Status.Revision+1, documentOf(t, c.API.Admin).Status.Revision)
}

var passField = regexp.MustCompile(`loginprobe kind=done .*pass=(\d+)`)

// lastPass is the number of the newest pass the probe reported on the console, or 0.
func lastPass(console string) int {
	last := 0
	for _, m := range passField.FindAllStringSubmatch(console, -1) {
		n, _ := strconv.Atoi(m[1])
		last = max(last, n)
	}
	return last
}

// loginLine is one line of naru-loginprobe on the console.
var loginLine = regexp.MustCompile(`loginprobe kind=(\S+)(.*)`)

// adrPorts are the TCP ports ADR 0047 lists a control-plane Gaon Node with the Node API as listening
// on, each named by its configuration: init's Node API (spec.api.listen, by default), gaon's API
// server, etcd's client and peer ports, and the registry. mvmd, the scheduler, the controllers and
// the Node Agent open no TCP port.
var adrPorts = []string{"50000", "6443", "2379", "2380", "5000"}

func TestNode_ANodeHasNoLoginPathNoShellNoGettyNoLoginNoSSHDNoReaderOfTheConsoleAndNoUnnamedPort(t *testing.T) {
	probe := node.ExtraService{Name: "loginprobe", Binary: path.Join(fixture.GuestRoot, "bin/naru-loginprobe")}
	c := start(t, node.ClusterSpec{N: 1, Gaon: true, NodeAPI: true, ExtraServices: []node.ExtraService{probe}})
	nd := c.Nodes[0]
	// pids and documentOf fail the test on an error, which a poll must not do while the Node is still
	// booting and its API refuses the connection, so the poll below starts once the API answers.
	eventually(t, "the Node API answers an admin", func() bool { return ctl(c.API.Admin, "version").Err == nil })
	eventually(t, "every service of node-1 is running", func() bool {
		return len(pids(t, c.API.Admin)) >= len(documentOf(t, c.API.Admin).Status.Services)
	})
	// The probe reports every few seconds: a pass that starts after the services run sees their ports.
	seen := lastPass(nd.Console())
	eventually(t, "the probe reports a pass after the services run", func() bool { return lastPass(nd.Console()) > seen+1 })

	lines := map[string][]string{}
	for _, m := range loginLine.FindAllStringSubmatch(nd.Console(), -1) {
		if !slices.Contains(lines[m[1]], m[2]) { // the kept log is re-printed, and a line may come twice
			lines[m[1]] = append(lines[m[1]], m[2])
		}
	}

	assert.Empty(t, lines["program"], "the image has no shell, getty, login or sshd: %v", lines["program"])
	assert.Empty(t, lines["tty-holder"], "no service holds a terminal: %v", lines["tty-holder"])
	assert.Empty(t, lines["reader"], "nothing reads the console: %v", lines["reader"])
	var ports []string
	for _, l := range lines["listener"] {
		ports = append(ports, regexp.MustCompile(`port=(\d+)`).FindStringSubmatch(l)[1])
	}
	require.Contains(t, ports, "50000", "the probe saw the Node API's listener, so it sees listeners")
	for _, p := range ports {
		assert.Contains(t, adrPorts, p, "port %s is not in ADR 0047's list: %v", p, lines["listener"])
	}
}
