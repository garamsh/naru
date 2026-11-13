package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The condition type of a NaruClusterDeclaration and the reasons it carries.
const (
	// DeclarationReconciled is True while the Cluster API objects match the declaration.
	DeclarationReconciled = "Reconciled"
	// ReasonApplied is the reason of a True DeclarationReconciled.
	ReasonApplied = "Applied"
	// ReasonApplyFailed is the reason when an object could not be created or updated.
	ReasonApplyFailed = "ApplyFailed"
)

// ControlPlaneDeclaration is the control plane of a declared cluster.
type ControlPlaneDeclaration struct {
	// replicas is the number of control-plane Nodes: 1 or 3.
	// +required
	// +kubebuilder:validation:Enum=1;3
	Replicas int32 `json:"replicas"`
}

// WorkersDeclaration is the workers of a declared cluster.
type WorkersDeclaration struct {
	// replicas is the number of worker Nodes.
	// +required
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`
}

// NodeDeclaration is the size of each Node of a declared cluster.
type NodeDeclaration struct {
	// cpu is the CPUs of a Node.
	// +required
	// +kubebuilder:validation:Minimum=1
	CPU int32 `json:"cpu"`

	// memory is the memory of a Node.
	// +required
	Memory resource.Quantity `json:"memory"`
}

// NaruClusterDeclarationSpec is one customer cluster.
type NaruClusterDeclarationSpec struct {
	// region is the region whose hosts run the cluster's Nodes. It cannot change.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region cannot change"
	Region string `json:"region"`

	// hostSelector selects the hosts, by their labels, that may run the cluster's Nodes.
	// +optional
	HostSelector metav1.LabelSelector `json:"hostSelector,omitempty,omitzero"`

	// controlPlane is the cluster's control plane.
	// +required
	ControlPlane ControlPlaneDeclaration `json:"controlPlane"`

	// workers is the cluster's workers.
	// +required
	Workers WorkersDeclaration `json:"workers"`

	// version is the Kubernetes version of every Node.
	// +required
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`
	Version string `json:"version"`

	// node is the size of every Node. It cannot change.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="node cannot change"
	Node NodeDeclaration `json:"node"`
}

// NaruClusterDeclarationStatus is what the controller observed of the declaration.
type NaruClusterDeclarationStatus struct {
	// observedGeneration is the generation of the declaration the conditions describe.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions holds Reconciled.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=8
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NaruClusterDeclaration declares one customer cluster. The Cluster API objects that realise it are
// named after it and owned by it.
// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=ncd,categories=naru
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 49",message="the name is at most 49 characters, as the objects built from it add a suffix"
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Reconciled",type=string,JSONPath=`.status.conditions[?(@.type=="Reconciled")].status`
type NaruClusterDeclaration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NaruClusterDeclarationSpec   `json:"spec,omitempty"`
	Status NaruClusterDeclarationStatus `json:"status,omitempty"`
}

// NaruClusterDeclarationList is a list of NaruClusterDeclaration.
// +kubebuilder:object:root=true
type NaruClusterDeclarationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruClusterDeclaration `json:"items"`
}
