// Package scheme builds the scheme the manager and the tests share: the built-in types, Cluster
// API's core types and naru's API group.
package scheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/garamsh/naru/cluster/internal/api/v1alpha1"
)

// New returns a scheme that knows every type the controllers read or write.
func New() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(clusterv1.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))
	return s
}
