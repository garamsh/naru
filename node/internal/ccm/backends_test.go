package ccm_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/garamsh/naru/node/internal/ccm"
)

func slice(endpoints ...discoveryv1.Endpoint) discoveryv1.EndpointSlice {
	return discoveryv1.EndpointSlice{Endpoints: endpoints}
}

func endpoint(nodeName *string, ready *bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{NodeName: nodeName, Conditions: discoveryv1.EndpointConditions{Ready: ready}}
}

func TestOnlyAReadyEndpointWithANodeNamesANode(t *testing.T) {
	got := ccm.ReadyNodeNames([]discoveryv1.EndpointSlice{
		slice(endpoint(ptr.To("ready"), ptr.To(true)), endpoint(ptr.To("unset"), nil), endpoint(ptr.To("terminating"), ptr.To(false)), endpoint(nil, ptr.To(true))),
		slice(endpoint(ptr.To("second"), ptr.To(true))),
	})
	require.Equal(t, map[string]bool{"ready": true, "unset": true, "second": true}, got, "an unset ready condition is ready; a false one and an endpoint with no Node are not")
}

func TestLocalKeepsTheNodesInTheSetAndClusterKeepsThemAll(t *testing.T) {
	nodes := []*v1.Node{node("a"), node("b")}
	local := service("d", "web", "")
	local.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	require.Equal(t, []*v1.Node{nodes[1]}, ccm.SelectNodes(local, nodes, map[string]bool{"b": true}))
	require.Equal(t, nodes, ccm.SelectNodes(service("d", "web", ""), nodes, map[string]bool{"b": true}), "control: the same set leaves Cluster alone")
}

func readySlice(ns, svc, nodeName string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: svc + "-" + nodeName, Labels: map[string]string{discoveryv1.LabelServiceName: svc}},
		Endpoints:  []discoveryv1.Endpoint{endpoint(ptr.To(nodeName), ptr.To(true))},
	}
}

func readyNode(name, ip string) *v1.Node {
	n := node(name, internal(ip))
	n.Status.Conditions = []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}
	return n
}

func TestAChangeOfTheSlicesStoresTheBackendsAgain(t *testing.T) {
	s := web()
	s.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	s.Spec.Ports[0].NodePort = 30443
	kube := fake.NewClientset(s, readyNode("a", "10.1.0.5"), readyNode("b", "10.1.0.6"), readySlice("d", "web", "a"))
	o := newFakeOrch()
	rs := ccm.NewResyncer(ccm.NewLoadBalancer(o, "lab", "lan", nil, ccm.KubeEndpoints(kube)), kube)

	require.NoError(t, rs.Sync(t.Context(), "d", "web"))
	require.Equal(t, []string{"10.1.0.5"}, o.applied[0].Backends)

	require.NoError(t, kube.DiscoveryV1().EndpointSlices("d").Delete(t.Context(), "web-a", metav1.DeleteOptions{}))
	_, err := kube.DiscoveryV1().EndpointSlices("d").Create(t.Context(), readySlice("d", "web", "b"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, rs.Sync(t.Context(), "d", "web"))
	require.Equal(t, []string{"10.1.0.6"}, o.applied[1].Backends, "the Pod moved to another Node")
}

func TestAResyncLeavesAServiceThatIsNotLocalAndOneThatIsGone(t *testing.T) {
	cluster := web()
	cluster.Spec.Ports[0].NodePort = 30443
	kube := fake.NewClientset(cluster, readyNode("a", "10.1.0.5"))
	o := newFakeOrch()
	rs := ccm.NewResyncer(ccm.NewLoadBalancer(o, "lab", "lan", nil, ccm.KubeEndpoints(kube)), kube)
	require.NoError(t, rs.Sync(t.Context(), "d", "web"))
	require.NoError(t, rs.Sync(t.Context(), "d", "gone"))
	require.Empty(t, o.applied)
}
