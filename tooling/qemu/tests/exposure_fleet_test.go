//go:build qemu_real

package tests_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/hostcli"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The exposure scenario (#275): a host takes a lease on a LAN address from the host API, forwards its
// port to a microVM of a network, and removes the address and the rule when the lease is not renewed.
// Two hosts show the address moving: the first releases it and the second takes it. The microVM is the
// probe of the network scenario. The DNAT is not read back from the host's nft counters, and the client guest on the LAN and the Pods that terminate TLS behind a
// PROXY protocol listener (ADR 0085, criterion 10) need harness additions that are not made; the
// header bytes and its position as the first write are the unit tests' (host/internal/network).
const (
	exposureAddr = "192.168.100.80"
	exposurePort = 80
	lanInterface = "eth0"
)

// withLANInterface gives naru-hostd the flag --lan-interface in the user-data: init writes no flag for it.
func withLANInterface(_ node.Config, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			var isHostd bool
			var args *yaml.Node
			for i := 0; i+1 < len(n.Content); i += 2 {
				switch n.Content[i].Value {
				case "name":
					isHostd = n.Content[i+1].Value == "naru-hostd"
				case "args":
					args = n.Content[i+1]
				}
			}
			if isHostd && args != nil && args.Kind == yaml.SequenceNode {
				args.Content = append(args.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "--lan-interface=" + lanInterface})
			}
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	visit(&doc)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}

func TestFleet_AHostHoldsALeasedLANAddressForwardsItToAMicroVMAndRemovesItWhenTheLeaseIsNotRenewed(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2, vmMiB: 1024, capture: true, userData: withLANInterface,
		services: []node.ExtraService{{Name: "naru94-nftprobe", Binary: nftProbe, Env: map[string]string{"PATH": "/bin"}}}})
	f.startOrch()
	f.enrollAndAdmit("eu-1", "disk=ssd")
	h1, h2 := 2, 3
	f.must(f.orch("network", "apply", "-f", f.networkFile(netA, prefixA)), "network apply "+netA)

	vm := f.netVMOn(h1, "a1", netA)
	ctx := context.Background()
	prov := func(i int) *hostapi.Client {
		c, err := hostcli.New(f.hostOfNode(i).Provisioner)
		require.NoError(t, err)
		return c
	}
	first, second := prov(h1), prov(h2)
	request := hostapi.ExposureRequest{
		Network: netA, Ports: []hostapi.ExposurePort{{Port: exposurePort, Protocol: "tcp", NodePort: 8080}},
		Backends: []string{vm.ip}, LeaseSeconds: 15,
	}

	// The lease is taken, and the host forwards the flow of the address to the microVM.
	e, err := first.PutExposure(ctx, exposureAddr, request)
	require.NoError(t, err)
	assert.Equal(t, exposureAddr, e.Address)
	eventually(t, "the host lists the lease", func() bool {
		got, err := first.Exposures(ctx)
		return err == nil && len(got) == 1 && got[0].Address == exposureAddr
	})

	// Renewed every 5 s, the lease outlives its 15 s.
	for range 4 {
		time.Sleep(5 * time.Second)
		_, err := first.PutExposure(ctx, exposureAddr, request)
		require.NoError(t, err)
	}
	got, err := first.Exposures(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 1, "the control: a renewed lease is still held after 20 s")

	// Released on the first host, taken on the second: the address moves.
	require.NoError(t, first.DeleteExposure(ctx, exposureAddr))
	got, err = first.Exposures(ctx)
	require.NoError(t, err)
	assert.Empty(t, got, "the first host holds nothing after the release")
	_, err = second.PutExposure(ctx, exposureAddr, request)
	require.NoError(t, err)
	got, err = second.Exposures(ctx)
	require.NoError(t, err)
	assert.Len(t, got, 1, "the second host holds the address")

	// Without a renewal the host removes the lease itself within the lease and a tick.
	eventually(t, "the second host drops the lease it is not renewing", func() bool {
		got, err := second.Exposures(ctx)
		return err == nil && len(got) == 0
	})
}
