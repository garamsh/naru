package ccm_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider/api"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/node/internal/ccm"
)

func web() *v1.Service {
	s := service("d", "web", "", v1.ServicePort{Port: 443, NodePort: 30443})
	s.UID = "0a1b2c3d-1111-2222-3333-444455556666"
	return s
}

func TestEnsureReturnsTheAddressOnceAHostAnnouncesIt(t *testing.T) {
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan")
	nodes := []*v1.Node{node("a", internal("10.1.0.5"))}

	st, err := lb.EnsureLoadBalancer(t.Context(), "c", web(), nodes)
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", st.Ingress[0].IP)
	require.Equal(t, "lab", o.applied[0].Network)
	require.Equal(t, "lan", o.applied[0].Pool)

	o.phase = orchapi.ExposurePending
	_, err = lb.EnsureLoadBalancer(t.Context(), "c", web(), nodes)
	var retry *api.RetryError
	require.ErrorAs(t, err, &retry, "an address nobody announces yet is no status")
}

func TestUpdateStoresTheNewNodesAndWaitsForNothing(t *testing.T) {
	o := newFakeOrch()
	o.phase = orchapi.ExposurePending
	lb := ccm.NewLoadBalancer(o, "lab", "lan")
	require.NoError(t, lb.UpdateLoadBalancer(t.Context(), "c", web(), []*v1.Node{node("a", internal("10.1.0.7"))}))
	require.Equal(t, []string{"10.1.0.7"}, o.applied[0].Backends)
	o.err = errors.New("orch down")
	require.Error(t, lb.UpdateLoadBalancer(t.Context(), "c", web(), []*v1.Node{node("a", internal("10.1.0.7"))}), "control: a real failure is returned")
}

func TestAServiceOfAnotherClassIsLeftToItsOwner(t *testing.T) {
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan")
	s := web()
	class := "metallb.io/l2"
	s.Spec.LoadBalancerClass = &class
	_, err := lb.EnsureLoadBalancer(t.Context(), "c", s, []*v1.Node{node("a", internal("10.1.0.5"))})
	require.ErrorIs(t, err, cloudprovider.ImplementedElsewhere)
	require.NoError(t, lb.EnsureLoadBalancerDeleted(t.Context(), "c", s))
	_, exists, err := lb.GetLoadBalancer(t.Context(), "c", s)
	require.NoError(t, err)
	require.False(t, exists)
	require.Empty(t, o.applied)
	require.Empty(t, o.deletes, "nothing was asked of orch for it")
	require.NoError(t, lb.EnsureLoadBalancerDeleted(t.Context(), "c", web()))
	require.Equal(t, []string{"d.web.0a1b2c3d"}, o.deletes, "control: the Service of the controller's own class is deleted")
}

func TestGetSaysWhetherTheExposureExists(t *testing.T) {
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan")
	_, exists, err := lb.GetLoadBalancer(t.Context(), "c", web())
	require.NoError(t, err)
	require.False(t, exists)
	_, err = lb.EnsureLoadBalancer(t.Context(), "c", web(), []*v1.Node{node("a", internal("10.1.0.5"))})
	require.NoError(t, err)
	st, exists, err := lb.GetLoadBalancer(t.Context(), "c", web())
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "192.168.0.80", st.Ingress[0].IP)
	o.err = errors.New("orch down")
	_, _, err = lb.GetLoadBalancer(t.Context(), "c", web())
	require.Error(t, err, "an orch that cannot be asked is not an exposure that is absent")
}

func TestASweepDeletesTheExposuresOfServicesThatAreGone(t *testing.T) {
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan")
	nodes := []*v1.Node{node("a", internal("10.1.0.5"))}
	_, err := lb.EnsureLoadBalancer(t.Context(), "c", web(), nodes)
	require.NoError(t, err)
	gone := service("d", "gone", "", v1.ServicePort{Port: 1, NodePort: 30001})
	gone.UID = "dddddddd-1111-2222-3333-444455556666"
	_, err = lb.EnsureLoadBalancer(t.Context(), "c", gone, nodes)
	require.NoError(t, err)

	p := ccm.New(ccm.Config{Network: "lab", Pool: "lan"}, o, discard())
	require.NoError(t, p.Sweep(t.Context(), []v1.Service{*web()}))
	require.Equal(t, []string{"d.gone.dddddddd"}, o.deletes)
	require.Contains(t, o.exps, "d.web.0a1b2c3d", "control: the live Service keeps its exposure")
}
