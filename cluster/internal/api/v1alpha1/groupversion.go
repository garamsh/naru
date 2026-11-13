package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version of every type in this package.
var GroupVersion = schema.GroupVersion{Group: "naru.garam.sh", Version: "v1alpha1"}

// SchemeBuilder collects the functions that add this package's types to a scheme.
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&NaruHost{}, &NaruHostList{},
		&NaruClusterDeclaration{}, &NaruClusterDeclarationList{},
		&NaruCluster{}, &NaruClusterList{},
		&NaruMachine{}, &NaruMachineList{},
		&NaruMachineTemplate{}, &NaruMachineTemplateList{},
		&NaruConfig{}, &NaruConfigList{},
		&NaruConfigTemplate{}, &NaruConfigTemplateList{},
		&NaruControlPlane{}, &NaruControlPlaneList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})

// AddToScheme adds this package's types to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme
