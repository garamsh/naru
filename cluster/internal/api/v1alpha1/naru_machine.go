package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NaruMachineSpec is one Node's microVM. It follows Cluster API's InfraMachine contract at contract
// version v1beta2. The Node's role, version and cluster come from its Machine.
type NaruMachineSpec struct {
	// providerID is the Node's provider ID, set by the infrastructure provider.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ProviderID string `json:"providerID,omitempty"`

	// cpu is the microVM's CPUs.
	// +required
	// +kubebuilder:validation:Minimum=1
	CPU int32 `json:"cpu"`

	// memory is the microVM's memory.
	// +required
	Memory resource.Quantity `json:"memory"`
}

// NaruMachineInitializationStatus is Cluster API's initialization contract of an InfraMachine.
// +kubebuilder:validation:MinProperties=1
type NaruMachineInitializationStatus struct {
	// provisioned is true when the microVM runs.
	// +optional
	Provisioned *bool `json:"provisioned,omitempty"`
}

// NaruMachineStatus is what the infrastructure provider observed of the microVM.
type NaruMachineStatus struct {
	// initialization is the Cluster API initialization contract.
	// +optional
	Initialization NaruMachineInitializationStatus `json:"initialization,omitempty,omitzero"`

	// host is the name of the NaruHost that runs the microVM. It is chosen once and never changes.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Host string `json:"host,omitempty"`

	// podCIDR is the Node's Pod range, in CIDR form, a /24 inside the cluster's prefix.
	// +optional
	// +kubebuilder:validation:MaxLength=64
	PodCIDR string `json:"podCIDR,omitempty"`

	// addresses are the Node's addresses.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// conditions holds Ready, which mirrors into the Machine's InfrastructureReady.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NaruMachine is one Node's microVM.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=narumachines,shortName=nm,scope=Namespaced,categories=cluster-api;naru
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.status.host`
// +kubebuilder:printcolumn:name="Provisioned",type=boolean,JSONPath=`.status.initialization.provisioned`
type NaruMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NaruMachineSpec   `json:"spec,omitempty"`
	Status NaruMachineStatus `json:"status,omitempty"`
}

// NaruMachineList is a list of NaruMachine.
// +kubebuilder:object:root=true
type NaruMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruMachine `json:"items"`
}

// NaruMachineTemplateResource is the template of a NaruMachine.
type NaruMachineTemplateResource struct {
	// metadata is the standard object's metadata of the NaruMachines made from the template.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the spec of the NaruMachines made from the template.
	// +required
	Spec NaruMachineSpec `json:"spec"`
}

// NaruMachineTemplateSpec is the spec of a NaruMachineTemplate.
type NaruMachineTemplateSpec struct {
	// template is what a NaruMachine is made from. It cannot change: a new size is a new template.
	// +required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="template cannot change"
	Template NaruMachineTemplateResource `json:"template"`
}

// NaruMachineTemplate is what a MachineDeployment or a NaruControlPlane makes NaruMachines from.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=narumachinetemplates,shortName=nmt,scope=Namespaced,categories=cluster-api;naru
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
type NaruMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NaruMachineTemplateSpec `json:"spec,omitempty"`
}

// NaruMachineTemplateList is a list of NaruMachineTemplate.
// +kubebuilder:object:root=true
type NaruMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruMachineTemplate `json:"items"`
}
