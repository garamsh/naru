package ccm

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Endpoints says which Nodes run a ready endpoint of a Service.
type Endpoints interface {
	// ReadyNodes is the set of names of the Nodes that run a ready endpoint of the Service.
	ReadyNodes(ctx context.Context, svc *v1.Service) (map[string]bool, error)
}

// SelectNodes is the Nodes the Service's traffic is sent to, by its externalTrafficPolicy: every Node
// for Cluster, and only those that run a ready endpoint of the Service for Local. It reads ends only
// for Local. It is the one place the policy is read.
func SelectNodes(ctx context.Context, svc *v1.Service, nodes []*v1.Node, ends Endpoints) ([]*v1.Node, error) {
	if !wantsLocal(svc) {
		return nodes, nil
	}
	ready, err := ends.ReadyNodes(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("endpoints of %s/%s: %w", svc.Namespace, svc.Name, err)
	}
	var out []*v1.Node
	for _, n := range nodes {
		if ready[n.Name] {
			out = append(out, n)
		}
	}
	return out, nil
}

// ReadyNodeNames is the Nodes that run an endpoint of the slices whose ready condition is true or unset
// (an unset condition means ready, as it does for kube-proxy). An endpoint with no Node is no Node's.
func ReadyNodeNames(slices []discoveryv1.EndpointSlice) map[string]bool {
	out := map[string]bool{}
	for i := range slices {
		for _, e := range slices[i].Endpoints {
			if e.NodeName == nil || (e.Conditions.Ready != nil && !*e.Conditions.Ready) {
				continue
			}
			out[*e.NodeName] = true
		}
	}
	return out
}

// kubeEndpoints reads the EndpointSlices of a Service from the API server.
type kubeEndpoints struct{ kube kubernetes.Interface }

func (k kubeEndpoints) ReadyNodes(ctx context.Context, svc *v1.Service) (map[string]bool, error) {
	list, err := k.kube.DiscoveryV1().EndpointSlices(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: discoveryv1.LabelServiceName + "=" + svc.Name})
	if err != nil {
		return nil, err
	}
	return ReadyNodeNames(list.Items), nil
}
