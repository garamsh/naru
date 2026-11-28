//go:build qemu_real

package tests_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The ingress scenario (#387, ADR 0112 item 7): of the hosts' own addresses on the mesh, only a
// proxy host's reaches a Machine, and only on the ports its ingress admits. Three hosts take mesh
// addresses: the first runs naru-proxy, the second runs two microVMs as the reconciler creates a
// Machine's, each admitting the mesh pool to one port (8080, where naru-entry's probe listens, and
// 9999, where nothing does), and the third runs neither. naru-meshprobe on each host connects from
// its mesh address to both microVMs' 8080: from the proxy host the first answers and the second,
// whose ingress admits another port, does not; from the third host neither answers, and the second
// host counts the drop. The first microVM also dials out of the host, which hostd's forward chain
// drops as before, and counts.
const ingressMeshPool = "10.99.0.8/29"

// ingressCandidates is every address the pool gives a host: the first is skipped.
var ingressCandidates = []string{"10.99.0.9", "10.99.0.10", "10.99.0.11", "10.99.0.12"}

// ingressVM creates on host Node i a microVM on no network, as the reconciler creates a Machine, with
// the ingress rules given and every egress, and returns its address once it runs.
func (f *fleet) ingressVM(i int, name string, ingress ...map[string]any) string {
	t := f.t
	body, err := json.Marshal(map[string]any{
		"name": name, "image": inputs.Image.Repository + "@" + inputs.Image.Digest,
		"cpu": 1, "memory": 256 << 20, "rootDiskBytes": 1 << 30,
		"networkPolicy": map[string]any{"ingress": ingress, "egress": []map[string]any{{"prefix": "0.0.0.0/0", "protocol": "any"}}},
	})
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), name+".json")
	require.NoError(t, os.WriteFile(file, body, 0o600))
	h := f.hostOfNode(i)
	f.must(hctl(h.Provisioner, []string{"microvm", "create"}, "-f", file), "microvm create "+name)
	var ip string
	eventually(t, name+" runs and has an address", func() bool {
		r := hctl(h.Reader, []string{"microvm", "get"}, name)
		var vm struct {
			State     string   `json:"state"`
			Addresses []string `json:"addresses"`
		}
		if r.Err != nil || json.Unmarshal([]byte(r.Stdout), &vm) != nil || vm.State != "running" || len(vm.Addresses) == 0 {
			return false
		}
		ip = strings.SplitN(vm.Addresses[0], "/", 2)[0]
		return true
	})
	return ip
}

func TestFleet_OnlyAProxyHostsMeshAddressReachesAMachineAndOnlyOnItsServicePorts(t *testing.T) {
	proxyHost, machineHost, plainHost := 2, 3, 4
	// mvmd gives a host's first microVMs the addresses after the gateway of its Machine prefix,
	// 10.77.<Node>.0/24; the probes are given them before the microVMs exist.
	open, closed := fmt.Sprintf("10.77.%d.2", machineHost), fmt.Sprintf("10.77.%d.3", machineHost)
	targets := append([]string{"tcp://" + open + ":" + probePort, "tcp://" + closed + ":" + probePort}, ingressCandidates...)
	cert, key := zonePair(t)
	f := newFleet(t, fleetOpts{controls: 1, hosts: 3, vmMiB: 1024,
		userData: func(c node.Config, file string) error {
			if c.Index != proxyHost {
				return nil
			}
			return addProxy(file, cert, key)
		},
		services: []node.ExtraService{
			{Name: "naru94-nftprobe", Binary: nftProbe, Env: map[string]string{"PATH": "/bin"}},
			{Name: "naru94-meshprobe", Binary: meshProbe, Args: append([]string{meshInterface, meshPort}, targets...), Env: map[string]string{"PATH": "/bin"}},
		}})
	f.startOrch()
	profile := filepath.Join(f.dir, "host-profile.yaml")
	f.must(nrun("gen", "host-profile", "--firecracker-sha256", inputs.FirecrackerSHA256,
		"--machine-pool", "10.77.0.0/16", "--mesh-pool", ingressMeshPool,
		"--orch-url", "https://"+nodeIP(1)+":"+orchPort, "--orch-ca-sha256", f.caSHA,
		"--host-management-prefix", managementPrefix, "--host-egress-interface", "eth0", "--host-tunnel-port", "51820",
		"--out", profile), "gen host-profile")
	f.must(f.orch("host", "profile", "set", "-f", profile), "host profile set")
	ids := f.enrollAndAdmit("eu-1")
	// A host's proxy is trusted with the role only once the operator grants it (ADR 0119).
	f.must(f.orch("host", "grant-proxy", ids[proxyHost]), "host grant-proxy")

	// The first microVM admits the mesh pool to 8080, and the harness's route to its probe, which
	// comes from the host's gateway; the second admits the mesh pool to 9999 alone.
	gateway := fmt.Sprintf("10.77.%d.1/32", machineHost)
	gotOpen := f.ingressVM(machineHost, "open",
		map[string]any{"prefix": ingressMeshPool, "protocol": "tcp", "ports": []map[string]any{{"first": 8080, "last": 8080}}},
		map[string]any{"prefix": gateway, "protocol": "tcp", "ports": []map[string]any{{"first": 8080, "last": 8080}}})
	gotClosed := f.ingressVM(machineHost, "closed",
		map[string]any{"prefix": ingressMeshPool, "protocol": "tcp", "ports": []map[string]any{{"first": 9999, "last": 9999}}})
	require.Equal(t, open, gotOpen, "the setup: the probes were given the first microVM's address")
	require.Equal(t, closed, gotClosed, "and the second's")

	answer := func(i int, target string) string {
		r, ok := meshOf(f.node(i))
		if !ok || r.self == "" {
			return ""
		}
		return r.answers["tcp://"+target+":"+probePort]
	}
	eventually(t, "the proxy host reaches the first microVM's service port from its mesh address", func() bool {
		return answer(proxyHost, open) == "open"
	})
	assert.Equal(t, "fail", answer(proxyHost, closed), "the second microVM's ingress admits another port")
	assert.Equal(t, "fail", answer(plainHost, open), "a host that runs no proxy does not reach the port mvmd admits for the mesh pool")
	assert.Equal(t, "fail", answer(plainHost, closed))

	machines := f.node(machineHost)
	eventually(t, "the Machine's host counts the other host's attempts", func() bool {
		return counter(machines, "a_host_to_a_microVM") > 0
	})

	// The forward chain's other drops still count: the first microVM is a Machine, no network's
	// member, so it leaves its host by the egress from its own address, but the other hosts are on the
	// management prefix, which no Machine reaches.
	addr, stop, err := f.hostOfNode(machineHost).RouteTo(context.Background(), open+":"+probePort)
	require.NoError(t, err)
	t.Cleanup(stop)
	vm := netVM{name: "open", ip: open, host: machineHost, base: "http://" + addr}
	before, forged := counter(machines, "a_Machine_to_management"), counter(machines, "forged_source")
	assert.False(t, vm.dials(t, nodeIP(plainHost)+":"+meshPort), "the microVM reaches no other host")
	eventually(t, "the forward chain counts the Machine's dial to the management prefix", func() bool {
		return counter(machines, "a_Machine_to_management") > before
	})
	assert.Equal(t, forged, counter(machines, "forged_source"), "a Machine's own address is no forged source")
}
