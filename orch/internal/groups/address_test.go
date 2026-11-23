package groups_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups"
)

// addressSpec is an OS-form group whose template lists the addresses of its members.
func addressSpec(name string, count int, addrs ...string) groups.GroupSpec {
	s := spec(name, count)
	s.Template = groups.Template{
		Form: groups.FormOS, CPUs: 1, MemoryMiB: 256, NodeVersion: "naru-node-v1", Addresses: addrs,
		Disks: []groups.Disk{{Name: "data", SizeBytes: 1 << 30}},
	}
	return s
}

func prefixedHost(id string, prefixes ...string) groups.Host {
	h := okHost(id)
	h.NodeVersions = []string{"naru-node-v1"}
	h.MachinePrefixes = prefixes
	return h
}

func TestPlace_AMemberTakesAHostWhoseMachinePrefixHoldsItsAddress(t *testing.T) {
	s := addressSpec("cp", 2, "10.77.1.10", "10.77.2.10")
	one, two := prefixedHost("one", "10.77.1.0/24"), prefixedHost("two", "10.77.2.0/24")

	got, err := groups.Place(s, groups.WantOf(s, "cp-1"), []groups.Host{two, one}, nil)
	require.NoError(t, err)
	assert.Equal(t, "one", got.ID)
	got, err = groups.Place(s, groups.WantOf(s, "cp-2"), []groups.Host{one, two}, nil)
	require.NoError(t, err)
	assert.Equal(t, "two", got.ID)

	// The control: the same host refuses the member whose address is outside its prefix, and says why.
	_, err = groups.Place(s, groups.WantOf(s, "cp-2"), []groups.Host{one}, nil)
	var pe *groups.PlacementError
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, err.Error(), "address 10.77.2.10 is outside the host's Machine prefixes")
}

func TestPlace_AMemberPastTheListedAddressesIsRefusedEverywhere(t *testing.T) {
	s := addressSpec("cp", 2, "10.77.1.10")

	_, err := groups.Place(s, groups.WantOf(s, "cp-2"), []groups.Host{prefixedHost("one", "10.77.1.0/24")}, nil)

	var pe *groups.PlacementError
	require.ErrorAs(t, err, &pe)
	assert.Contains(t, err.Error(), "member cp-2 has no address")
}

func TestPlace_ATemplateWithNoAddressesPlacesOnAnyHost(t *testing.T) {
	s := addressSpec("cp", 1)
	_, err := groups.Place(s, groups.WantOf(s, "cp-1"), []groups.Host{prefixedHost("one", "10.77.1.0/24")}, nil)
	require.NoError(t, err)
}

func TestAddresses_TheHostIsAskedForTheAddressOfEachMemberByItsNumber(t *testing.T) {
	w := newWorld(t)
	h1 := w.addHost("one", "kr-1", nil, "naru-node-v1")
	h1.host.MachinePrefixes = []string{"10.77.1.0/24"}
	h2 := w.addHost("two", "kr-1", nil, "naru-node-v1")
	h2.host.MachinePrefixes = []string{"10.77.2.0/24"}
	w.setGroup(addressSpec("cp", 2, "10.77.1.10", "10.77.2.10"))
	e := w.engine()
	w.settle(e, 5)

	for id, want := range map[string]string{"cp-1": "10.77.1.10", "cp-2": "10.77.2.10"} {
		m, host := w.machine(id)
		require.NotNil(t, m, id)
		assert.Equal(t, want, m.spec.Address, id)
		assert.Equal(t, map[string]string{"cp-1": "one", "cp-2": "two"}[id], host, "the member is on the host whose prefix holds its address")
	}
}

func TestValidate_AddressesAreDistinctIPv4AddressesOfAnOSFormTemplate(t *testing.T) {
	require.NoError(t, addressSpec("cp", 2, "10.77.1.10", "10.77.1.11").Validate())
	for name, s := range map[string]groups.GroupSpec{
		"repeated": addressSpec("cp", 2, "10.77.1.10", "10.77.1.10"),
		"IPv6":     addressSpec("cp", 1, "fd00::10"),
		"not one":  addressSpec("cp", 1, "ten"),
		"a prefix": addressSpec("cp", 1, "10.77.1.0/24"),
	} {
		assert.Error(t, s.Validate(), name)
	}
	image := spec("web", 1)
	image.Template.Addresses = []string{"10.77.1.10"}
	assert.Error(t, image.Validate(), "the image form takes no address")
}
