package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NaruClusterSpec is the infrastructure of one cluster: where its Nodes may run and how its
// control plane is reached. It follows Cluster API's InfraCluster contract at contract version v1beta2.
type NaruClusterSpec struct {
	// region is the region whose hosts run the cluster's Nodes.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Region string `json:"region"`

	// hostSelector selects the hosts, by their labels, that may run the cluster's Nodes.
	// +optional
	HostSelector metav1.LabelSelector `json:"hostSelector,omitempty,omitzero"`

	// controlPlaneEndpoint is the endpoint of the cluster's API server, set by the infrastructure
	// provider from the cluster's prefix.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`
}

// NaruClusterInitializationStatus is Cluster API's initialization contract of an InfraCluster.
// +kubebuilder:validation:MinProperties=1
type NaruClusterInitializationStatus struct {
	// provisioned is true when the cluster's infrastructure is provisioned.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// NaruClusterStatus is what the infrastructure provider observed of the cluster.
type NaruClusterStatus struct {
	// initialization is the Cluster API initialization contract.
	// +optional
	Initialization NaruClusterInitializationStatus `json:"initialization,omitempty,omitzero"`

	// prefix is the cluster's address range, taken from the region's pool, in CIDR form. The Nodes'
	// addresses and Pod ranges, and the API endpoint, are inside it.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	Prefix string `json:"prefix,omitempty"`

	// conditions holds Ready, which mirrors into the Cluster's InfrastructureReady.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NaruCluster is the infrastructure of one cluster.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=naruclusters,shortName=nc,scope=Namespaced,categories=cluster-api;naru
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.status.prefix`
type NaruCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NaruClusterSpec   `json:"spec,omitempty"`
	Status NaruClusterStatus `json:"status,omitempty"`
}

// NaruClusterList is a list of NaruCluster.
// +kubebuilder:object:root=true
type NaruClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruCluster `json:"items"`
}
