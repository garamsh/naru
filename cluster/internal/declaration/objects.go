package declaration

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/naru/cluster/internal/api/v1alpha1"
)

// poolLabel tells the Machines of the workers from the control plane's in one cluster.
const poolLabel = "naru.garam.sh/pool"

// The names of the objects built from a declaration called name. The cluster, its infrastructure
// and its control plane take the declaration's name; the others carry a suffix, which is why the
// name is capped (the API's validation does it).
func controlPlaneTemplateName(name string) string { return name + "-control-plane" }
func workersName(name string) string              { return name + "-workers" }

// ref refers to an object of naru's API group by kind and name, as Cluster API's contract does.
func ref(kind, name string) clusterv1.ContractVersionedObjectReference {
	return clusterv1.ContractVersionedObjectReference{Kind: kind, Name: name, APIGroup: v1alpha1.GroupVersion.Group}
}

// setNaruCluster makes c carry the declaration's region and selector. It leaves the endpoint and
// the status to the infrastructure provider.
func setNaruCluster(c *v1alpha1.NaruCluster, d *v1alpha1.NaruClusterDeclaration) {
	c.Spec.Region = d.Spec.Region
	c.Spec.HostSelector = *d.Spec.HostSelector.DeepCopy()
}

// setMachineTemplate gives t the size of a Node. A template cannot change, so it is set only when
// t is new.
func setMachineTemplate(t *v1alpha1.NaruMachineTemplate, d *v1alpha1.NaruClusterDeclaration) {
	if !t.CreationTimestamp.IsZero() {
		return
	}
	t.Spec.Template.Spec = v1alpha1.NaruMachineSpec{CPU: d.Spec.Node.CPU, Memory: d.Spec.Node.Memory.DeepCopy()}
}

// setControlPlane makes cp the control plane the declaration asks for.
func setControlPlane(cp *v1alpha1.NaruControlPlane, d *v1alpha1.NaruClusterDeclaration) {
	cp.Spec.Replicas = ptr.To(d.Spec.ControlPlane.Replicas)
	cp.Spec.Version = d.Spec.Version
	cp.Spec.MachineTemplate.Spec.InfrastructureRef = ref("NaruMachineTemplate", controlPlaneTemplateName(d.Name))
}

// setCluster makes c point at the declaration's infrastructure and control plane.
func setCluster(c *clusterv1.Cluster, d *v1alpha1.NaruClusterDeclaration) {
	c.Spec.InfrastructureRef = ref("NaruCluster", d.Name)
	c.Spec.ControlPlaneRef = ref("NaruControlPlane", d.Name)
}

// setMachineDeployment makes md the workers the declaration asks for.
func setMachineDeployment(md *clusterv1.MachineDeployment, d *v1alpha1.NaruClusterDeclaration) {
	labels := map[string]string{clusterv1.ClusterNameLabel: d.Name, poolLabel: "workers"}
	md.Spec.ClusterName = d.Name
	md.Spec.Replicas = ptr.To(d.Spec.Workers.Replicas)
	md.Spec.Selector = metav1.LabelSelector{MatchLabels: labels}
	md.Spec.Template.Labels = labels
	md.Spec.Template.Spec.ClusterName = d.Name
	md.Spec.Template.Spec.Version = d.Spec.Version
	md.Spec.Template.Spec.Bootstrap.ConfigRef = ref("NaruConfigTemplate", workersName(d.Name))
	md.Spec.Template.Spec.InfrastructureRef = ref("NaruMachineTemplate", workersName(d.Name))
}

// fill sets the fields of obj the declaration decides. The two NaruMachineTemplates are the same
// size: a Node's.
func fill(obj client.Object, d *v1alpha1.NaruClusterDeclaration) {
	switch o := obj.(type) {
	case *v1alpha1.NaruCluster:
		setNaruCluster(o, d)
	case *v1alpha1.NaruMachineTemplate:
		setMachineTemplate(o, d)
	case *v1alpha1.NaruControlPlane:
		setControlPlane(o, d)
	case *clusterv1.Cluster:
		setCluster(o, d)
	case *clusterv1.MachineDeployment:
		setMachineDeployment(o, d)
	}
}
