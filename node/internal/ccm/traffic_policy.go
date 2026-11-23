package ccm

import (
	v1 "k8s.io/api/core/v1"
)

const (
	// ReasonLocalPolicy is the reason of the Warning event a Service that sets externalTrafficPolicy
	// Local receives.
	ReasonLocalPolicy = "ExternalTrafficPolicyLocalNotSupported"

	localPolicyMessage = "naru-ccm does not support externalTrafficPolicy Local: the address forwards to the NodePort of every Node, whether or not it runs an endpoint (ADR 0085 item 8)"
)

// LocalPolicyNotice is the event a Service that sets externalTrafficPolicy Local is told, and false for
// a Service that does not: the exposure it gets is the cluster behaviour's, so the Service is told.
func LocalPolicyNotice(svc *v1.Service) (reason, message string, ok bool) {
	if svc.Spec.ExternalTrafficPolicy != v1.ServiceExternalTrafficPolicyLocal {
		return "", "", false
	}
	return ReasonLocalPolicy, localPolicyMessage, true
}
