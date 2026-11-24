package layout_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/layout"
)

func node(t *testing.T, change func(*layout.Node)) layout.Node {
	t.Helper()
	d := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	n := layout.Node{
		Name: "node-1", Address: netip.MustParsePrefix("192.168.0.10/24"),
		MachinePrefix: netip.MustParsePrefix("10.200.1.0/24"), FirecrackerSHA256: d,
	}
	change(&n)
	return n
}

var awsProfile = &layout.AWS{Region: "eu-west-1", TrustAnchorArn: "arn:ta", ProfileArn: "arn:p", RoleArn: "arn:r"}

func TestUserData_RefusesWhatTheNodeCannotRun(t *testing.T) {
	_, err := node(t, func(*layout.Node) {}).UserData()
	require.NoError(t, err, "the control: the Node alone is accepted")
	host, _ := hostNode(t)
	host.AWS = awsProfile
	_, err = host.UserData()
	require.NoError(t, err, "the control: a host with an IAM Roles Anywhere profile is accepted")
	for name, c := range map[string]struct {
		change func(*layout.Node)
		want   string
	}{
		"spec.aws without the host API": {func(n *layout.Node) { n.AWS = awsProfile }, "serves the host API"},
		"a joined host with no certificate": {func(n *layout.Node) {
			n.Joined, n.Host = true, &layout.Host{Region: "eu-1"}
		}, "the certificate it was issued"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := node(t, c.change).UserData()
			assert.ErrorContains(t, err, c.want)
		})
	}
}

func TestUserData_AJoinedHostHasNoPeers(t *testing.T) {
	n, err := profile(t).Node(joined(t))
	require.NoError(t, err)
	_, err = n.UserData()
	require.NoError(t, err, "the control: the host orch builds is accepted")

	n.Peers = []layout.Peer{{Address: netip.MustParseAddr("192.0.2.11"), MachinePrefix: netip.MustParsePrefix("10.200.2.0/24")}}

	_, err = n.UserData()
	assert.ErrorContains(t, err, "has no peers")
}

// naru runs no control plane (ADR 0114): the user-data names no spec.controlPlane, which init
// refuses as true, and a Node runs mvmd, naru-hostd when it serves the host API, and the registry,
// with no file but theirs.
func TestUserData_NamesNoControlPlaneAndOnlyTheNodesOwnServicesAndFiles(t *testing.T) {
	host, _ := hostNode(t)
	for name, c := range map[string]struct {
		n        layout.Node
		services []string
		files    []string
	}{
		"a Node":                      {node(t, func(*layout.Node) {}), []string{"mvmd", "registry"}, []string{layout.MvmdConfig}},
		"a Node without the registry": {node(t, func(n *layout.Node) { n.NoRegistry = true }), []string{"mvmd"}, []string{layout.MvmdConfig}},
		"a host": {host, []string{"mvmd", layout.HostService, "registry"}, []string{
			layout.MvmdConfig, layout.HostDir + "/tls.crt", layout.HostDir + "/tls.key", layout.HostDir + "/client-ca.crt",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := c.n.UserData()
			require.NoError(t, err)

			assert.NotContains(t, string(data), "controlPlane")
			p := parse(t, data)
			var services, files []string
			for _, s := range p.Spec.Services {
				services = append(services, s.Name)
			}
			for _, f := range p.Spec.Files {
				files = append(files, f.Path)
			}
			assert.Equal(t, c.services, services)
			assert.Equal(t, c.files, files)
		})
	}
}
