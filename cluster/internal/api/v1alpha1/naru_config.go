package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// NaruConfigSpec is the bootstrap configuration of one Node. It follows Cluster API's
// BootstrapConfig contract at contract version v1beta2. It has no field yet: the Node's role,
// version and cluster come from its Machine.
type NaruConfigSpec struct{}

// NaruConfigInitializationStatus is Cluster API's initialization contract of a BootstrapConfig.
// +kubebuilder:validation:MinProperties=1
type NaruConfigInitializationStatus struct {
	// dataSecretCreated is true when the bootstrap data Secret exists.
	// +optional
	DataSecretCreated *bool `json:"dataSecretCreated,omitempty"`
}

// NaruConfigStatus is what the bootstrap provider observed of the configuration.
type NaruConfigStatus struct {
	// initialization is the Cluster API initialization contract.
	// +optional
	Initialization NaruConfigInitializationStatus `json:"initialization,omitempty,omitzero"`

	// dataSecretName is the Secret that holds the Node's user-data under the key value.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	DataSecretName string `json:"dataSecretName,omitempty"`

	// conditions holds Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NaruConfig is the bootstrap configuration of one Node.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=naruconfigs,shortName=ncfg,scope=Namespaced,categories=cluster-api;naru
// +kubebuilder:subresource:status
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
type NaruConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NaruConfigSpec   `json:"spec,omitempty"`
	Status NaruConfigStatus `json:"status,omitempty"`
}

// NaruConfigList is a list of NaruConfig.
// +kubebuilder:object:root=true
type NaruConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruConfig `json:"items"`
}

// NaruConfigTemplateResource is the template of a NaruConfig.
type NaruConfigTemplateResource struct {
	// metadata is the standard object's metadata of the NaruConfigs made from the template.
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec is the spec of the NaruConfigs made from the template.
	// +required
	Spec NaruConfigSpec `json:"spec"`
}

// NaruConfigTemplateSpec is the spec of a NaruConfigTemplate.
type NaruConfigTemplateSpec struct {
	// template is what a NaruConfig is made from.
	// +required
	Template NaruConfigTemplateResource `json:"template"`
}

// NaruConfigTemplate is what a MachineDeployment makes NaruConfigs from.
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=naruconfigtemplates,shortName=ncfgt,scope=Namespaced,categories=cluster-api;naru
// +kubebuilder:metadata:labels="cluster.x-k8s.io/v1beta2=v1alpha1"
type NaruConfigTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NaruConfigTemplateSpec `json:"spec,omitempty"`
}

// NaruConfigTemplateList is a list of NaruConfigTemplate.
// +kubebuilder:object:root=true
type NaruConfigTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NaruConfigTemplate `json:"items"`
}
