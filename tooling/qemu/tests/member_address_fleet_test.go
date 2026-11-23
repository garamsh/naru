//go:build qemu_real

package tests_test

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// A group template that lists the addresses of its members gives each member the address of its
// number: the host asks mvmd for it (ADR 0099). A group whose address no host's Machine prefix
// holds is held with the reason and no microVM is created for it.
func TestFleet_AGroupWhoseTemplateListsItsMembersAddressesGivesEachMemberItsOwn(t *testing.T) {
	image, guestDir := os.Getenv("NARU_IMAGE_DIR"), os.Getenv("NARU_GUEST_DIR")
	if image == "" || guestDir == "" {
		t.Skip("NARU_IMAGE_DIR and NARU_GUEST_DIR name the files make image wrote")
	}
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	api := f.hostOfNode(h).Addr
	operator := f.operator("ops-1", "operator")
	call := func(words ...string) result {
		args := append(append([]string{}, words...), "--identity", operator.file, "--passphrase-file", f.pass, "--host-ca", f.c.HostCA, "--endpoint", api)
		return nrun(args...)
	}

	assets := t.TempDir()
	require.NoError(t, release.WriteBootSet(assets, "qemu-addresses",
		filepath.Join(guestDir, "boot-sets", release.NodeBootSet, "vmlinux"), filepath.Join(image, "node-initrd.cpio.gz")))
	const set = "naru-node-qemu-addresses"
	f.must(call("host", "boot-set", "add", filepath.Join(assets, set+".json")), "add the boot set")
	var host struct {
		Mvmd struct {
			MachinePrefixes []string `json:"machinePrefixes"`
		} `json:"mvmd"`
	}
	eventually(t, "the host lists the boot set and its Machine prefix", func() bool {
		r := call("host")
		return r.Err == nil && strings.Contains(r.Stdout, set) && json.Unmarshal([]byte(r.Stdout), &host) == nil && len(host.Mvmd.MachinePrefixes) > 0
	})
	prefix := netip.MustParsePrefix(host.Mvmd.MachinePrefixes[0])
	first := prefix.Masked().Addr().Next().Next().Next() // well inside the prefix, clear of the network address and the gateway
	second := first.Next()

	group := func(name string, addrs ...string) string {
		return f.file(name+".yaml", fmt.Sprintf(`name: %s
count: %d
region: eu-1
template:
  form: os
  cpus: 1
  memoryMiB: 256
  nodeVersion: %s
  addresses: [%s]
  disks:
    - {name: data, sizeBytes: 1073741824}
rollout: {maxUnavailable: 0, maxSurge: 0}
`, name, len(addrs), set, strings.Join(addrs, ", ")))
	}
	addressOf := func(name string) string {
		var vm struct {
			Addresses []string `json:"addresses"`
		}
		r := call("microvm", "get", name)
		if r.Err != nil || json.Unmarshal([]byte(r.Stdout), &vm) != nil || len(vm.Addresses) == 0 {
			return ""
		}
		a, _, _ := strings.Cut(vm.Addresses[0], "/")
		return a
	}

	f.must(f.orch("group", "apply", "-f", group("cpa", first.String())), "apply the first group")
	f.must(f.orch("group", "apply", "-f", group("cpb", second.String())), "apply the second group")
	eventually(t, "both members run", func() bool {
		a, okA := f.group(1, "cpa")
		b, okB := f.group(1, "cpb")
		return okA && okB && a.count("running") == 1 && b.count("running") == 1
	})
	assert.Equal(t, first.String(), addressOf("cpa-1"), "the member has the address its template lists")
	assert.Equal(t, second.String(), addressOf("cpb-1"))

	// The control: an address outside every Machine prefix of the host holds the group.
	f.must(f.orch("group", "apply", "-f", group("out", "10.99.99.9")), "apply a group whose address no host holds")
	eventually(t, "the group is held for the address", func() bool {
		r := f.orch("group", "get", "out")
		return r.Err == nil && strings.Contains(r.Stdout, "held at ") && strings.Contains(r.Stdout, "outside the host's Machine prefixes")
	})
	assert.Empty(t, addressOf("out-1"), "no microVM was made for it")
}
