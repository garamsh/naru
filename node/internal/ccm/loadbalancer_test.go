package ccm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
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
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil)
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
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil)
	require.NoError(t, lb.UpdateLoadBalancer(t.Context(), "c", web(), []*v1.Node{node("a", internal("10.1.0.7"))}))
	require.Equal(t, []string{"10.1.0.7"}, o.applied[0].Backends)
	o.err = errors.New("orch down")
	require.Error(t, lb.UpdateLoadBalancer(t.Context(), "c", web(), []*v1.Node{node("a", internal("10.1.0.7"))}), "control: a real failure is returned")
}

func TestAServiceWithAClassIsFulfilledToo(t *testing.T) {
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil)
	s := web()
	class := "metallb.io/l2"
	s.Spec.LoadBalancerClass = &class
	_, err := lb.EnsureLoadBalancer(t.Context(), "c", s, []*v1.Node{node("a", internal("10.1.0.5"))})
	require.NoError(t, err, "naru-ccm is the cluster's one LoadBalancer implementation (ADR 0093); the class is the service controller's to filter")
	require.Len(t, o.applied, 1)
}

func TestGetSaysWhetherTheExposureExists(t *testing.T) {
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil)
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
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil)
	nodes := []*v1.Node{node("a", internal("10.1.0.5"))}
	_, err := lb.EnsureLoadBalancer(t.Context(), "c", web(), nodes)
	require.NoError(t, err)
	gone := service("d", "gone", "", v1.ServicePort{Port: 1, NodePort: 30001})
	gone.UID = "dddddddd-1111-2222-3333-444455556666"
	_, err = lb.EnsureLoadBalancer(t.Context(), "c", gone, nodes)
	require.NoError(t, err)

	p := ccm.New(ccm.Config{Network: "lab", Pool: "lan"}, o, discard())
	require.NoError(t, p.Sweep(t.Context(), func(context.Context) ([]v1.Service, error) { return []v1.Service{*web()}, nil }))
	require.Equal(t, []string{"d.gone.dddddddd"}, o.deletes)
	require.Contains(t, o.exps, "d.web.0a1b2c3d", "control: the live Service keeps its exposure")
}

func TestASweepKeepsTheExposureOfAServiceMadeWhileItListsThem(t *testing.T) {
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil)
	nodes := []*v1.Node{node("a", internal("10.1.0.5"))}
	p := ccm.New(ccm.Config{Network: "lab", Pool: "lan"}, o, discard())
	made := service("d", "made", "", v1.ServicePort{Port: 2, NodePort: 30002})
	made.UID = "eeeeeeee-1111-2222-3333-444455556666"
	require.NoError(t, p.Sweep(t.Context(), func(ctx context.Context) ([]v1.Service, error) {
		_, err := lb.EnsureLoadBalancer(ctx, "c", made, nodes) // made after the exposures were listed, missing from this list
		return nil, err
	}))
	require.Empty(t, o.deletes, "the exposure that appeared after the exposures were listed is not released")
	require.Contains(t, o.exps, "d.made.eeeeeeee")
	require.NoError(t, p.Sweep(t.Context(), func(context.Context) ([]v1.Service, error) { return nil, nil }))
	require.Equal(t, []string{"d.made.eeeeeeee"}, o.deletes, "control: with no such Service the next sweep releases it")
}

func TestAServiceThatAsksForLocalIsToldItGetsTheClusterBehaviour(t *testing.T) {
	nodes := []*v1.Node{node("a", internal("10.1.0.5")), node("b", internal("10.1.0.6"))}
	for policy, wantEvent := range map[v1.ServiceExternalTrafficPolicy]bool{v1.ServiceExternalTrafficPolicyLocal: true, v1.ServiceExternalTrafficPolicyCluster: false, "": false} {
		o := newFakeOrch()
		rec := record.NewFakeRecorder(4)
		lb := ccm.NewLoadBalancer(o, "lab", "lan", rec)
		s := web()
		s.Spec.ExternalTrafficPolicy = policy

		_, err := lb.EnsureLoadBalancer(t.Context(), "c", s, nodes)
		require.NoError(t, err, "policy %q is fulfilled either way", policy)
		require.Equal(t, []string{"10.1.0.5", "10.1.0.6"}, o.applied[0].Backends, "every Node is a backend whatever the policy")
		if wantEvent {
			require.Len(t, rec.Events, 1, "policy %q", policy)
			require.Contains(t, <-rec.Events, "Warning "+ccm.ReasonLocalPolicy)
		} else {
			require.Empty(t, rec.Events, "control: policy %q is told nothing", policy)
		}
	}
}

func TestAServiceThatAsksForLocalIsToldWhileItsAddressIsStillPending(t *testing.T) {
	o := newFakeOrch()
	o.phase = orchapi.ExposurePending
	rec := record.NewFakeRecorder(4)
	lb := ccm.NewLoadBalancer(o, "lab", "lan", rec)
	s := web()
	s.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	_, err := lb.EnsureLoadBalancer(t.Context(), "c", s, []*v1.Node{node("a", internal("10.1.0.5"))})
	var retry *api.RetryError
	require.ErrorAs(t, err, &retry)
	require.Len(t, rec.Events, 1)
}
