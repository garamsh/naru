package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The condition type of a NaruHost and the reasons it carries.
const (
	// HostReady is True while the host API answers.
	HostReady = "Ready"
	// ReasonAnswered is the reason of a True HostReady.
	ReasonAnswered = "Answered"
	// ReasonUnreachable is the reason when the host API gave no answer.
	ReasonUnreachable = "Unreachable"
	// ReasonRejected is the reason when the host API refused the client pair's role.
	ReasonRejected = "Rejected"
	// ReasonClientPairInvalid is the reason when the Secret does not hold a usable client pair.
	ReasonClientPairInvalid = "ClientPairInvalid"
)

// SecretReference names a Secret in a namespace.
type SecretReference struct {
	// name is the Secret's name.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// namespace is the Secret's namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
}

// NaruHostSpec is where a host's API is and which client pair reaches it.
type NaruHostSpec struct {
	// address is the host API's address as host:port.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=261
	Address string `json:"address"`

	// clientSecretRef names the Secret holding the client pair the host API's mutual TLS asks for:
	// tls.crt and tls.key, and ca.crt, the authority that signed the host's serving certificate.
	// +required
	ClientSecretRef SecretReference `json:"clientSecretRef"`
}

// HostResources are amounts of a host's CPU, memory and state disk.
type HostResources struct {
	// cpu is a count of CPUs.
	// +optional
	CPU resource.Quantity `json:"cpu,omitzero"`

	// memory is in bytes.
	// +optional
	Memory resource.Quantity `json:"memory,omitzero"`

	// disk is in bytes.
	// +optional
	Disk resource.Quantity `json:"disk,omitzero"`
}

// NaruHostStatus is what the host API last answered.
type NaruHostStatus struct {
	// region is the host's region.
	// +optional
	Region string `json:"region,omitempty"`

	// labels are the host's labels, which a hostSelector matches.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// capacity is what the host has.
	// +optional
	Capacity HostResources `json:"capacity,omitempty,omitzero"`

	// free is what the host has left for new microVMs.
	// +optional
	Free HostResources `json:"free,omitempty,omitzero"`

	// conditions holds Ready: True while the host API answers, False with the reason when it does not.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NaruHost is one host in naru's inventory.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,categories=naru
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.status.region`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
type NaruHost struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NaruHostSpec   `json:"spec,omitempty"`
	Status NaruHostStatus `json:"status,omitempty"`
}

// NaruHostList is a list of NaruHost.
// +kubebuilder:object:root=true
type NaruHostList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruHost `json:"items"`
}
