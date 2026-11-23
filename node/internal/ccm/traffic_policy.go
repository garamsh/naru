package ccm

import (
	v1 "k8s.io/api/core/v1"
)

const (
	// ReasonLocalPolicy is the reason of the event a Service that sets externalTrafficPolicy Local
	// receives.
	ReasonLocalPolicy = "ExternalTrafficPolicyLocal"

	localPolicyMessage = "naru-ccm sends this Service's external traffic only to the Nodes that run a ready endpoint of it, as externalTrafficPolicy Local asks; " +
		"the client's address reaches the backend with the annotation " + ProxyProtocolAnnotation + ": v2, not through Local (ADR 0097)"
)

// wantsLocal reports whether the Service asks that only Nodes with a local endpoint receive its traffic.
func wantsLocal(svc *v1.Service) bool {
	return svc.Spec.ExternalTrafficPolicy == v1.ServiceExternalTrafficPolicyLocal
}

// LocalPolicyNotice is the event a Service that sets externalTrafficPolicy Local is told, and false for
// a Service that does not.
func LocalPolicyNotice(svc *v1.Service) (reason, message string, ok bool) {
	if !wantsLocal(svc) {
		return "", "", false
	}
	return ReasonLocalPolicy, localPolicyMessage, true
}
