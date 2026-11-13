package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NaruControlPlaneMachineDeletion holds the options of the deletion of a control-plane Machine.
// +kubebuilder:validation:MinProperties=1
type NaruControlPlaneMachineDeletion struct {
	// nodeDrainTimeoutSeconds is how long draining a control-plane Node may take; 0 is no limit.
	// +optional
	// +kubebuilder:validation:Minimum=0
	NodeDrainTimeoutSeconds *int32 `json:"nodeDrainTimeoutSeconds,omitempty"`

	// nodeVolumeDetachTimeoutSeconds is how long waiting for volumes to detach may take; 0 is no limit.
	// +optional
	// +kubebuilder:validation:Minimum=0
	NodeVolumeDetachTimeoutSeconds *int32 `json:"nodeVolumeDetachTimeoutSeconds,omitempty"`

	// nodeDeletionTimeoutSeconds is how long deleting the Node object may be retried; 0 is without end.
	// +optional
	// +kubebuilder:validation:Minimum=0
	NodeDeletionTimeoutSeconds *int32 `json:"nodeDeletionTimeoutSeconds,omitempty"`
}

// NaruControlPlaneMachineTemplateSpec is the spec of the Machines of a control plane.
type NaruControlPlaneMachineTemplateSpec struct {
	// infrastructureRef refers to the NaruMachineTemplate the control-plane Machines are made from.
	// +required
	InfrastructureRef clusterv1.ContractVersionedObjectReference `json:"infrastructureRef"`

	// deletion holds the options of deleting a Machine.
	// +optional
	Deletion NaruControlPlaneMachineDeletion `json:"deletion,omitempty,omitzero"`
}

// NaruControlPlaneMachineTemplate is how the Machines of a control plane are shaped.
type NaruControlPlaneMachineTemplate struct {
	// metadata is the standard object's metadata of the Machines.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the spec of the Machines.
	// +required
	Spec NaruControlPlaneMachineTemplateSpec `json:"spec"`
}

// NaruControlPlaneSpec is a cluster's control plane. It follows Cluster API's ControlPlane contract
// at contract version v1beta2, with Machines.
type NaruControlPlaneSpec struct {
	// replicas is the number of control-plane Machines: 1 or 3.
	// +required
	// +kubebuilder:validation:Enum=1;3
	Replicas *int32 `json:"replicas"`

	// version is the Kubernetes version of the control plane.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	Version string `json:"version"`

	// machineTemplate is how the control plane's Machines are shaped.
	// +required
	MachineTemplate NaruControlPlaneMachineTemplate `json:"machineTemplate"`

	// controlPlaneEndpoint is the endpoint of the cluster's API server, set by the control plane provider.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`
}

// NaruControlPlaneInitializationStatus is Cluster API's initialization contract of a ControlPlane.
// +kubebuilder:validation:MinProperties=1
type NaruControlPlaneInitializationStatus struct {
	// controlPlaneInitialized is true when the cluster's API server accepts requests.
	// +optional
	ControlPlaneInitialized *bool `json:"controlPlaneInitialized,omitempty"`
}

// NaruControlPlaneStatus is what the control plane provider observed of the control plane.
type NaruControlPlaneStatus struct {
	// initialization is the Cluster API initialization contract.
	// +optional
	Initialization NaruControlPlaneInitializationStatus `json:"initialization,omitempty,omitzero"`

	// selector is the label selector of the control plane's Machines, for the scale subresource.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Selector string `json:"selector,omitempty"`

	// replicas is the number of Machines the control plane targets.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// readyReplicas is the number of Machines whose Ready condition is true.
	// +optional
	ReadyReplicas *int32 `json:"readyReplicas,omitempty"`

	// availableReplicas is the number of Machines whose Available condition is true.
	// +optional
	AvailableReplicas *int32 `json:"availableReplicas,omitempty"`

	// upToDateReplicas is the number of Machines whose UpToDate condition is true.
	// +optional
	UpToDateReplicas *int32 `json:"upToDateReplicas,omitempty"`

	// versions are the Kubernetes versions of the Machines, from the oldest to the newest.
	// +optional
	// +listType=map
	// +listMapKey=version
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	Versions []clusterv1.StatusVersion `json:"versions,omitempty"`

	// conditions holds Available.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NaruControlPlane is a cluster's control plane.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=narucontrolplanes,shortName=ncp,scope=Namespaced,categories=cluster-api;naru
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
type NaruControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NaruControlPlaneSpec   `json:"spec,omitempty"`
	Status NaruControlPlaneStatus `json:"status,omitempty"`
}

// NaruControlPlaneList is a list of NaruControlPlane.
// +kubebuilder:object:root=true
type NaruControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruControlPlane `json:"items"`
}
