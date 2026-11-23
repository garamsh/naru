package ccm

import "k8s.io/client-go/kubernetes"

// KubeEndpoints is the Endpoints the provider reads from an API server.
func KubeEndpoints(kube kubernetes.Interface) Endpoints { return kubeEndpoints{kube: kube} }
