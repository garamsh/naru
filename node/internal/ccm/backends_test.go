package ccm_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/cloud-provider/api"
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
	ends := fakeEndpoints{ready: map[string]bool{"b": true}}
	got, err := ccm.SelectNodes(t.Context(), local, nodes, ends)
	require.NoError(t, err)
	require.Equal(t, []*v1.Node{nodes[1]}, got)
	got, err = ccm.SelectNodes(t.Context(), service("d", "web", ""), nodes, ends)
	require.NoError(t, err)
	require.Equal(t, nodes, got, "control: the same set leaves Cluster alone")
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
	nodes := []*v1.Node{readyNode("a", "10.1.0.5"), readyNode("b", "10.1.0.6")}
	kube := fake.NewClientset(s, nodes[0], nodes[1], readySlice("d", "web", "a"))
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil, ccm.KubeEndpoints(kube))
	rs := ccm.NewResyncer(lb, kube)

	_, err := lb.EnsureLoadBalancer(t.Context(), "c", s, nodes)
	require.NoError(t, err)
	require.Equal(t, []string{"10.1.0.5"}, o.applied[0].Backends)

	require.NoError(t, kube.DiscoveryV1().EndpointSlices("d").Delete(t.Context(), "web-a", metav1.DeleteOptions{}))
	_, err = kube.DiscoveryV1().EndpointSlices("d").Create(t.Context(), readySlice("d", "web", "b"), metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, rs.Sync(t.Context(), "d", "web"))
	require.Equal(t, []string{"10.1.0.6"}, o.applied[1].Backends, "the Pod moved to another Node")
}

func TestAResyncStoresTheSameBackendsAsTheServiceControllerDid(t *testing.T) {
	s := web()
	s.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	s.Spec.Ports[0].NodePort = 30443
	notReady := node("b", internal("10.1.0.6")) // the service controller does not filter on Ready
	notReady.Status.Conditions = []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}}
	included := readyNode("c", "10.1.0.7") // =false includes the Node
	included.Labels = map[string]string{"node.kubernetes.io/exclude-from-external-load-balancers": "false"}
	nodes := []*v1.Node{readyNode("a", "10.1.0.5"), notReady, included}
	kube := fake.NewClientset(s, nodes[0], nodes[1], nodes[2], readySlice("d", "web", "a"), readySlice("d", "web", "b"), readySlice("d", "web", "c"))
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil, ccm.KubeEndpoints(kube))
	rs := ccm.NewResyncer(lb, kube)

	require.NoError(t, rs.Sync(t.Context(), "d", "web"))
	require.Empty(t, o.applied, "control: before the controller passed any Node there is nothing to store")

	_, err := lb.EnsureLoadBalancer(t.Context(), "c", s, nodes)
	require.NoError(t, err)
	require.NoError(t, rs.Sync(t.Context(), "d", "web"))
	require.Len(t, o.applied, 2)
	require.Equal(t, []string{"10.1.0.5", "10.1.0.6", "10.1.0.7"}, o.applied[0].Backends)
	require.Equal(t, o.applied[0].Backends, o.applied[1].Backends, "the exposure does not flip between the two writers")
}

func TestRunStoresTheBackendsOfTheServiceWhoseSliceAppears(t *testing.T) {
	local := func(s *v1.Service) *v1.Service {
		s.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
		return s
	}
	webSvc := local(web())
	other := local(service("d", "other", "ffffffff-1111-2222-3333-444455556666", v1.ServicePort{Port: 80, NodePort: 30080}))
	// The control's endpoint runs on a Node the controller never passed, so storing it would be a bug.
	kube := fake.NewClientset(webSvc, other)
	o := newFakeOrch()
	lb := ccm.NewLoadBalancer(o, "lab", "lan", nil, ccm.KubeEndpoints(kube))
	nodes := []*v1.Node{readyNode("a", "10.1.0.5"), readyNode("b", "10.1.0.6")}
	for _, s := range []*v1.Service{webSvc, other} {
		_, err := lb.EnsureLoadBalancer(t.Context(), "c", s, nodes)
		var retry *api.RetryError
		require.ErrorAs(t, err, &retry, "the controller's own pass found no endpoint in the Nodes it passed")
	}
	require.Empty(t, o.applied)
	// The slices exist before Run lists them, so the informer reports each as added.
	for _, sl := range []*discoveryv1.EndpointSlice{readySlice("d", "other", "z"), readySlice("d", "web", "b")} {
		_, err := kube.DiscoveryV1().EndpointSlices("d").Create(t.Context(), sl, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ccm.NewResyncer(lb, kube).Run(ctx, func(key string, err error) { t.Errorf("resync %s: %v", key, err) })
	}()
	t.Cleanup(func() { cancel(); <-done })

	require.Eventually(t, func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return len(o.applied) > 0
	}, 10*time.Second, 20*time.Millisecond, "the slice for web is read and web stored again")
	o.mu.Lock()
	defer o.mu.Unlock()
	require.Equal(t, []string{"10.1.0.6"}, o.applied[0].Backends)
	require.Len(t, o.applied, 1, "control: the slice of the other Service stores nothing")
	require.Contains(t, o.exps, ccm.ExposureID(webSvc))
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
