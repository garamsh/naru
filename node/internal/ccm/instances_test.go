package ccm_test

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/node/internal/ccm"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func members() []orchapi.NetworkMember {
	return []orchapi.NetworkMember{
		{Name: "cp-0", Host: "h1", Phase: "running", Reported: true, Running: true, Address: "10.1.0.5", Region: "kr", Zone: "z1"},
		{Name: "cp-1", Host: "h2", Phase: "running", Reported: true, Running: false, Address: "10.1.0.6"},
		{Name: "cp-2", Host: "h3", Phase: "running"},
	}
}

func TestAMemberNoHostReportedStillExistsAndIsNotShutDown(t *testing.T) {
	o := newFakeOrch()
	o.members = members()
	in := ccm.NewInstances(o, "lab")
	for name, want := range map[string]struct{ exists, shutdown bool }{
		"cp-0": {true, false}, "cp-1": {true, true}, "cp-2": {true, false}, "gone": {false, false},
	} {
		n := node(name)
		exists, err := in.InstanceExists(t.Context(), n)
		require.NoError(t, err)
		require.Equal(t, want.exists, exists, "%s exists", name)
		shutdown, err := in.InstanceShutdown(t.Context(), n)
		require.NoError(t, err)
		require.Equal(t, want.shutdown, shutdown, "%s shut down", name)
	}
	require.Equal(t, "lab", o.memberOf)
}

func TestAnOrchThatCannotBeAskedDeletesNoNode(t *testing.T) {
	o := newFakeOrch()
	o.err = errors.New("orch down")
	exists, err := ccm.NewInstances(o, "lab").InstanceExists(t.Context(), node("cp-0"))
	require.Error(t, err)
	require.False(t, exists)
}

func TestMetadataNamesTheProviderIDTheAddressAndTheHostsPlace(t *testing.T) {
	o := newFakeOrch()
	o.members = members()
	in := ccm.NewInstances(o, "lab")
	md, err := in.InstanceMetadata(t.Context(), node("cp-0"))
	require.NoError(t, err)
	require.Equal(t, "naru://h1/cp-0", md.ProviderID)
	require.Equal(t, []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.1.0.5"}}, md.NodeAddresses)
	require.Equal(t, "kr", md.Region)
	require.Equal(t, "z1", md.Zone)

	_, err = in.InstanceMetadata(t.Context(), node("gone"))
	require.ErrorIs(t, err, cloudprovider.InstanceNotFound)
	_, err = in.InstanceMetadata(t.Context(), node("cp-2"))
	require.Error(t, err, "a member whose host did not report it has no address to name")
}

func TestTheProviderIDNamesTheMemberWhenTheNodeHasOne(t *testing.T) {
	o := newFakeOrch()
	o.members = members()
	in := ccm.NewInstances(o, "lab")
	n := node("renamed")
	n.Spec.ProviderID = "naru://h1/cp-0"
	exists, err := in.InstanceExists(t.Context(), n)
	require.NoError(t, err)
	require.True(t, exists, "found by the provider ID, not the Node's own name")
	n.Spec.ProviderID = "aws://i-1"
	_, err = in.InstanceExists(t.Context(), n)
	require.Error(t, err, "a provider ID of another scheme is no member")
}

func TestTheProviderServesTheNodeControllerAlwaysAndTheServiceControllerUnlessOff(t *testing.T) {
	off := false
	for _, c := range []struct {
		cfg ccm.Config
		lb  bool
	}{{ccm.Config{}, true}, {ccm.Config{Loadbalancer: &off}, false}} {
		p := ccm.New(c.cfg, newFakeOrch(), discard())
		_, lb := p.LoadBalancer()
		require.Equal(t, c.lb, lb)
		_, nodes := p.InstancesV2()
		require.True(t, nodes)
		_, v1ok := p.Instances()
		require.False(t, v1ok)
	}
}
